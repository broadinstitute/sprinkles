package monitor

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/firestore"
	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Firestore operation kinds recorded by FirestoreCallStats.
const (
	// FirestoreOpGet: read documents by ID (BatchGetDocuments; also what
	// DocumentRef.Get uses).
	FirestoreOpGet = "get"
	// FirestoreOpQuery: a query returning documents, filtered or not
	// (RunQuery).
	FirestoreOpQuery = "query"
	// FirestoreOpAggregate: a server-side count/sum/avg (RunAggregationQuery).
	FirestoreOpAggregate = "aggregate"
	// FirestoreOpWrite: create, set, or update of a document (a Commit write
	// that isn't a delete; includes writes issued through a WriteBatch).
	FirestoreOpWrite = "write"
	// FirestoreOpDelete: delete of a document (a Commit delete write).
	FirestoreOpDelete = "delete"
	// FirestoreOpListen: a snapshot-listener stream (Listen).
	FirestoreOpListen = "listen"
	// FirestoreOpTransaction: BeginTransaction / Rollback bookkeeping RPCs.
	FirestoreOpTransaction = "transaction"
	// FirestoreOpOther: any RPC this file doesn't classify.
	FirestoreOpOther = "other"
)

// FirestoreCallKey identifies one bucket of counted Firestore traffic.
type FirestoreCallKey struct {
	// Poller is the monitor poller (or other activity) that was running when
	// the call was made, or "" if the call wasn't labelled; see
	// WithFirestoreLabel.
	Poller string
	// Collection is the immediate collection ID, e.g. "WorkPool". Empty for
	// RPCs that don't target one (transactions, unclassified calls).
	Collection string
	Op         string
}

// FirestoreCallCount is the traffic recorded for one FirestoreCallKey.
type FirestoreCallCount struct {
	FirestoreCallKey
	// Calls is the number of RPCs. A Commit that writes several documents in
	// one collection counts once.
	Calls int64
	// Docs is the number of documents read (what Firestore bills for reads:
	// each document returned by a query/get, each aggregation result) or
	// written/deleted.
	Docs int64
}

// FirestoreCallStats counts the Firestore calls made through a client that
// was created with ClientOptions. It works at the gRPC layer, so it sees
// every call that client makes however it was issued (adapters, event
// publisher, task queue, ...) without each call site needing to opt in.
// Use a dedicated client for the monitor so the dashboard's own Firestore
// traffic isn't counted. Safe for concurrent use; methods are no-ops on a
// nil receiver.
type FirestoreCallStats struct {
	mu      sync.Mutex
	since   time.Time
	buckets map[FirestoreCallKey]*FirestoreCallCount
}

func NewFirestoreCallStats() *FirestoreCallStats {
	return &FirestoreCallStats{
		since:   time.Now(),
		buckets: map[FirestoreCallKey]*FirestoreCallCount{},
	}
}

// NewClient creates a Firestore client whose calls are counted. The returned
// cleanup func must be called after the client is closed.
//
// This can't be done with client options alone: when FIRESTORE_EMULATOR_HOST
// is set the Firestore library dials its own connection and ignores gRPC dial
// options, so in that case we dial the emulator ourselves (with the same
// credentials the library would use) and hand the connection in.
func (s *FirestoreCallStats) NewClient(ctx context.Context, project, database string) (*firestore.Client, func(), error) {
	dialOpts := []grpc.DialOption{
		grpc.WithChainUnaryInterceptor(s.unaryInterceptor),
		grpc.WithChainStreamInterceptor(s.streamInterceptor),
	}
	addr := os.Getenv("FIRESTORE_EMULATOR_HOST")
	if addr == "" {
		client, err := firestore.NewClientWithDatabase(ctx, project, database,
			option.WithGRPCDialOption(dialOpts[0]), option.WithGRPCDialOption(dialOpts[1]))
		return client, func() {}, err
	}
	conn, err := grpc.NewClient("passthrough:///"+addr, append(dialOpts,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(emulatorCreds{}))...)
	if err != nil {
		return nil, nil, fmt.Errorf("dialing firestore emulator %s: %w", addr, err)
	}
	client, err := firestore.NewClientWithDatabase(ctx, project, database, option.WithGRPCConn(conn))
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	return client, func() { conn.Close() }, nil
}

// emulatorCreds mirrors the unexported credentials the Firestore library
// uses against the emulator ("Bearer owner" is accepted as admin).
type emulatorCreds struct{}

func (emulatorCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer owner"}, nil
}
func (emulatorCreds) RequireTransportSecurity() bool { return false }

// Snapshot returns the counts so far, sorted by poller, collection, op, and
// the time counting started.
func (s *FirestoreCallStats) Snapshot() (counts []FirestoreCallCount, since time.Time) {
	if s == nil {
		return nil, time.Time{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	counts = make([]FirestoreCallCount, 0, len(s.buckets))
	for _, c := range s.buckets {
		counts = append(counts, *c)
	}
	sort.Slice(counts, func(i, j int) bool {
		a, b := counts[i].FirestoreCallKey, counts[j].FirestoreCallKey
		if a.Poller != b.Poller {
			return a.Poller < b.Poller
		}
		if a.Collection != b.Collection {
			return a.Collection < b.Collection
		}
		return a.Op < b.Op
	})
	return counts, s.since
}

func (s *FirestoreCallStats) add(poller, collection, op string, calls, docs int64) {
	k := FirestoreCallKey{Poller: poller, Collection: collection, Op: op}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.buckets[k]
	if !ok {
		c = &FirestoreCallCount{FirestoreCallKey: k}
		s.buckets[k] = c
	}
	c.Calls += calls
	c.Docs += docs
}

type firestoreLabelKey struct{}

// WithFirestoreLabel returns a context whose Firestore calls are attributed
// to label (normally a poller name) in FirestoreCallStats.
func WithFirestoreLabel(ctx context.Context, label string) context.Context {
	return context.WithValue(ctx, firestoreLabelKey{}, label)
}

func firestoreLabel(ctx context.Context) string {
	l, _ := ctx.Value(firestoreLabelKey{}).(string)
	return l
}

// collectionOfPath returns the collection ID of a document resource path
// such as "projects/p/databases/d/documents/WorkPool/wp-1".
func collectionOfPath(path string) string {
	_, rest, ok := strings.Cut(path, "/documents/")
	if !ok {
		return ""
	}
	segs := strings.Split(rest, "/")
	if len(segs) < 2 {
		return ""
	}
	return segs[len(segs)-2]
}

func collectionOfQuery(q *firestorepb.StructuredQuery) string {
	if q == nil || len(q.From) == 0 {
		return ""
	}
	return q.From[0].CollectionId
}

func (s *FirestoreCallStats) unaryInterceptor(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	// Count attempts, including failed ones: a failed RPC can still cost.
	err := invoker(ctx, method, req, reply, cc, opts...)
	label := firestoreLabel(ctx)
	switch r := req.(type) {
	case *firestorepb.CommitRequest:
		s.recordCommit(label, r)
	case *firestorepb.BeginTransactionRequest, *firestorepb.RollbackRequest:
		s.add(label, "", FirestoreOpTransaction, 1, 0)
	default:
		s.add(label, "", FirestoreOpOther, 1, 0)
	}
	return err
}

// recordCommit counts each write by its target collection and kind. One
// Commit counts as one call per distinct (collection, op) it contains.
func (s *FirestoreCallStats) recordCommit(label string, r *firestorepb.CommitRequest) {
	type bucket struct{ collection, op string }
	docs := map[bucket]int64{}
	for _, w := range r.Writes {
		switch op := w.Operation.(type) {
		case *firestorepb.Write_Delete:
			docs[bucket{collectionOfPath(op.Delete), FirestoreOpDelete}]++
		case *firestorepb.Write_Update:
			docs[bucket{collectionOfPath(op.Update.GetName()), FirestoreOpWrite}]++
		case *firestorepb.Write_Transform:
			docs[bucket{collectionOfPath(op.Transform.GetDocument()), FirestoreOpWrite}]++
		}
	}
	if len(docs) == 0 {
		s.add(label, "", FirestoreOpWrite, 1, 0)
		return
	}
	for b, n := range docs {
		s.add(label, b.collection, b.op, 1, n)
	}
}

func (s *FirestoreCallStats) streamInterceptor(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	cs, err := streamer(ctx, desc, cc, method, opts...)
	if err != nil {
		return cs, err
	}
	return &countingStream{ClientStream: cs, stats: s, label: firestoreLabel(ctx)}, nil
}

// countingStream records a streaming call when its request is sent (which
// tells us the op and collection) and the documents as they are received.
type countingStream struct {
	grpc.ClientStream
	stats *FirestoreCallStats
	label string

	mu         sync.Mutex
	op         string
	collection string
}

func (c *countingStream) SendMsg(m any) error {
	err := c.ClientStream.SendMsg(m)
	c.mu.Lock()
	defer c.mu.Unlock()
	switch r := m.(type) {
	case *firestorepb.RunQueryRequest:
		c.op, c.collection = FirestoreOpQuery, collectionOfQuery(r.GetStructuredQuery())
		c.stats.add(c.label, c.collection, c.op, 1, 0)
	case *firestorepb.RunAggregationQueryRequest:
		c.op = FirestoreOpAggregate
		c.collection = collectionOfQuery(r.GetStructuredAggregationQuery().GetStructuredQuery())
		c.stats.add(c.label, c.collection, c.op, 1, 0)
	case *firestorepb.BatchGetDocumentsRequest:
		c.op = FirestoreOpGet
		if len(r.Documents) > 0 {
			c.collection = collectionOfPath(r.Documents[0])
		}
		c.stats.add(c.label, c.collection, c.op, 1, 0)
	case *firestorepb.ListenRequest:
		// A listener is long-lived; count each target registration.
		c.op = FirestoreOpListen
		c.stats.add(c.label, "", c.op, 1, 0)
	default:
		if c.op == "" {
			c.op = FirestoreOpOther
			c.stats.add(c.label, "", c.op, 1, 0)
		}
	}
	return err
}

func (c *countingStream) RecvMsg(m any) error {
	err := c.ClientStream.RecvMsg(m)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch r := m.(type) {
	case *firestorepb.RunQueryResponse:
		if r.GetDocument() != nil {
			c.stats.add(c.label, c.collection, FirestoreOpQuery, 0, 1)
		}
	case *firestorepb.RunAggregationQueryResponse:
		if r.GetResult() != nil {
			c.stats.add(c.label, c.collection, FirestoreOpAggregate, 0, 1)
		}
	case *firestorepb.BatchGetDocumentsResponse:
		// A missing document is still billed as a read.
		path := r.GetMissing()
		if f := r.GetFound(); f != nil {
			path = f.GetName()
		}
		c.stats.add(c.label, collectionOfPath(path), FirestoreOpGet, 0, 1)
	case *firestorepb.ListenResponse:
		if dc := r.GetDocumentChange(); dc != nil {
			c.stats.add(c.label, collectionOfPath(dc.GetDocument().GetName()), FirestoreOpListen, 0, 1)
		}
	}
	return nil
}
