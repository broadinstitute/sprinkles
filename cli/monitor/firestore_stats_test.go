package monitor

import (
	"context"
	"io"
	"testing"

	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

const docBase = "projects/p/databases/d/documents/"

func countsByKey(s *FirestoreCallStats) map[FirestoreCallKey]FirestoreCallCount {
	counts, _ := s.Snapshot()
	m := map[FirestoreCallKey]FirestoreCallCount{}
	for _, c := range counts {
		m[c.FirestoreCallKey] = c
	}
	return m
}

func TestFirestoreCallStats_CommitCountsWritesAndDeletesByCollection(t *testing.T) {
	s := NewFirestoreCallStats()
	ctx := WithFirestoreLabel(context.Background(), "poller-a")
	req := &firestorepb.CommitRequest{Writes: []*firestorepb.Write{
		{Operation: &firestorepb.Write_Update{Update: &firestorepb.Document{Name: docBase + "WorkPool/wp1"}}},
		{Operation: &firestorepb.Write_Update{Update: &firestorepb.Document{Name: docBase + "WorkPool/wp2"}}},
		{Operation: &firestorepb.Write_Delete{Delete: docBase + "Workers/w1"}},
	}}
	invoker := func(context.Context, string, any, any, *grpc.ClientConn, ...grpc.CallOption) error { return nil }
	require.NoError(t, s.unaryInterceptor(ctx, "/google.firestore.v1.Firestore/Commit", req, nil, nil, invoker))

	got := countsByKey(s)
	assert.Equal(t, FirestoreCallCount{FirestoreCallKey{"poller-a", "WorkPool", FirestoreOpWrite}, 1, 2},
		got[FirestoreCallKey{"poller-a", "WorkPool", FirestoreOpWrite}])
	assert.Equal(t, FirestoreCallCount{FirestoreCallKey{"poller-a", "Workers", FirestoreOpDelete}, 1, 1},
		got[FirestoreCallKey{"poller-a", "Workers", FirestoreOpDelete}])
	assert.Len(t, got, 2)
}

// fakeStream replays canned responses.
type fakeStream struct {
	grpc.ClientStream
	sent []any
	recv []proto.Message
}

func (f *fakeStream) SendMsg(m any) error { f.sent = append(f.sent, m); return nil }
func (f *fakeStream) RecvMsg(m any) error {
	if len(f.recv) == 0 {
		return io.EOF
	}
	proto.Merge(m.(proto.Message), f.recv[0])
	f.recv = f.recv[1:]
	return nil
}

func runStream(t *testing.T, s *FirestoreCallStats, label string, req proto.Message, resp []proto.Message, newResp func() proto.Message) {
	t.Helper()
	fs := &fakeStream{recv: resp}
	streamer := func(context.Context, *grpc.StreamDesc, *grpc.ClientConn, string, ...grpc.CallOption) (grpc.ClientStream, error) {
		return fs, nil
	}
	cs, err := s.streamInterceptor(WithFirestoreLabel(context.Background(), label), nil, nil, "m", streamer)
	require.NoError(t, err)
	require.NoError(t, cs.SendMsg(req))
	for {
		if err := cs.RecvMsg(newResp()); err != nil {
			break
		}
	}
}

func TestFirestoreCallStats_QueryCountsCallAndDocuments(t *testing.T) {
	s := NewFirestoreCallStats()
	req := &firestorepb.RunQueryRequest{QueryType: &firestorepb.RunQueryRequest_StructuredQuery{
		StructuredQuery: &firestorepb.StructuredQuery{From: []*firestorepb.StructuredQuery_CollectionSelector{{CollectionId: "Tasks"}}},
	}}
	doc := &firestorepb.RunQueryResponse{Document: &firestorepb.Document{Name: docBase + "Tasks/t1"}}
	// A response with no document (e.g. a trailing read-time marker) isn't a billed read.
	empty := &firestorepb.RunQueryResponse{}
	runStream(t, s, "p", req, []proto.Message{doc, doc, empty}, func() proto.Message { return &firestorepb.RunQueryResponse{} })

	k := FirestoreCallKey{"p", "Tasks", FirestoreOpQuery}
	assert.Equal(t, FirestoreCallCount{k, 1, 2}, countsByKey(s)[k])
}

func TestFirestoreCallStats_BatchGetCountsMissingAsRead(t *testing.T) {
	s := NewFirestoreCallStats()
	req := &firestorepb.BatchGetDocumentsRequest{Documents: []string{docBase + "WorkPool/wp1"}}
	found := &firestorepb.BatchGetDocumentsResponse{Result: &firestorepb.BatchGetDocumentsResponse_Found{
		Found: &firestorepb.Document{Name: docBase + "WorkPool/wp1"}}}
	missing := &firestorepb.BatchGetDocumentsResponse{Result: &firestorepb.BatchGetDocumentsResponse_Missing{
		Missing: docBase + "WorkPool/wp2"}}
	runStream(t, s, "", req, []proto.Message{found, missing}, func() proto.Message { return &firestorepb.BatchGetDocumentsResponse{} })

	k := FirestoreCallKey{"", "WorkPool", FirestoreOpGet}
	assert.Equal(t, FirestoreCallCount{k, 1, 2}, countsByKey(s)[k])
}

func TestFirestoreCallStats_NilReceiverIsSafe(t *testing.T) {
	var s *FirestoreCallStats
	counts, _ := s.Snapshot()
	assert.Empty(t, counts)
}

func TestRunPollerLabelsFirestoreCalls(t *testing.T) {
	w := newWorld()
	var got string
	w.A.runPoller(context.Background(), PollerJobSummary, func(ctx context.Context) error {
		got = firestoreLabel(ctx)
		return nil
	})
	assert.Equal(t, PollerJobSummary, got)
}
