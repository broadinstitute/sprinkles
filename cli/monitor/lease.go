package monitor

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

// The monitor assumes it is the only one running against a Firestore
// database: it keeps in-memory state, and two monitors would provision
// duplicate VMs and overwrite each other's summaries. A lease document makes
// that assumption enforced rather than hoped for: a monitor must hold the
// lease to run, renews it while running, and stops if it can't prove it still
// holds it.

const (
	leaseCollection = "MonitorLease"
	// leaseDocID is the single document in leaseCollection.
	leaseDocID = "monitor"

	// CollectionMonitorLease is exported for functional tests.
	CollectionMonitorLease = leaseCollection

	// defaultLeaseTTL is how long a lease lasts without being renewed, so how
	// long a crashed monitor blocks its replacement (a clean shutdown releases
	// the lease and blocks nothing). The holder renews every ttl/3 (5 minutes
	// for the default) and stops itself if it hasn't managed to renew for
	// ttl*2/3 (10 minutes), so it always stops before the lease can be taken
	// over.
	defaultLeaseTTL = 15 * time.Minute

	// leaseFailureRetryInterval is how soon a failed renewal is retried. The
	// normal interval is long, so without this a single blip would cost a whole
	// interval of the margin; this gives several attempts before giving up.
	leaseFailureRetryInterval = 30 * time.Second

	// leaseCallTimeout bounds one renewal call (or the normal interval if
	// that is shorter), so a hung call can't use up the margin.
	leaseCallTimeout = 30 * time.Second

	// defaultLeaseRetryInterval is how often AcquireLease retries while
	// another holder's lease is still live.
	defaultLeaseRetryInterval = 30 * time.Second
)

// ErrLeaseLost means the lease is no longer ours: another monitor took it.
var ErrLeaseLost = errors.New("monitor lease lost")

// LeaseHeldError is returned when another monitor holds a live lease.
type LeaseHeldError struct {
	HolderID    string
	Description string
	ExpiresAt   time.Time
}

func (e *LeaseHeldError) Error() string {
	return fmt.Sprintf("another monitor holds the lease (%s, id %s), expiring at %s",
		e.Description, e.HolderID, e.ExpiresAt.Format(time.RFC3339))
}

// LeaseClaim identifies a would-be holder.
type LeaseClaim struct {
	HolderID string
	// Description is human-readable (host, pid) and only used in messages.
	Description string
}

// LeaseStore reads and writes the monitor lease document. All comparisons use
// the now passed in, so holders on different machines need reasonably
// synchronized clocks; the margin built into the renewal schedule (see
// defaultLeaseTTL) tolerates skew of up to about ttl/3.
type LeaseStore interface {
	// TryAcquire takes the lease if it is free, expired, or already ours,
	// setting it to expire at now+ttl. Otherwise it returns *LeaseHeldError.
	TryAcquire(ctx context.Context, claim LeaseClaim, now time.Time, ttl time.Duration) error
	// Renew extends our lease to now+ttl. It returns ErrLeaseLost if another
	// holder has taken it.
	Renew(ctx context.Context, holderID string, now time.Time, ttl time.Duration) error
	// Release gives up the lease if we still hold it; otherwise it does nothing.
	Release(ctx context.Context, holderID string) error
}

// ----- FirestoreLeaseStore -----

type firestoreLease struct {
	HolderID    string    `firestore:"holder_id"`
	Description string    `firestore:"description"`
	AcquiredAt  time.Time `firestore:"acquired_at"`
	ExpiresAt   time.Time `firestore:"expires_at"`
}

type FirestoreLeaseStore struct {
	fs *firestore.Client
}

func NewFirestoreLeaseStore(fs *firestore.Client) *FirestoreLeaseStore {
	return &FirestoreLeaseStore{fs: fs}
}

func (s *FirestoreLeaseStore) ref() *firestore.DocumentRef {
	return s.fs.Collection(leaseCollection).Doc(leaseDocID)
}

// current returns the lease document, or nil if there isn't one.
func (s *FirestoreLeaseStore) current(tx *firestore.Transaction) (*firestoreLease, error) {
	snap, err := tx.Get(s.ref())
	if err != nil {
		if grpcstatus.Code(err) == codes.NotFound {
			return nil, nil
		}
		return nil, err
	}
	var cur firestoreLease
	if err := snap.DataTo(&cur); err != nil {
		return nil, err
	}
	return &cur, nil
}

func (s *FirestoreLeaseStore) TryAcquire(ctx context.Context, claim LeaseClaim, now time.Time, ttl time.Duration) error {
	return s.fs.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		cur, err := s.current(tx)
		if err != nil {
			return err
		}
		acquiredAt := now
		if cur != nil {
			if cur.HolderID != claim.HolderID && cur.ExpiresAt.After(now) {
				return &LeaseHeldError{HolderID: cur.HolderID, Description: cur.Description, ExpiresAt: cur.ExpiresAt}
			}
			if cur.HolderID == claim.HolderID {
				acquiredAt = cur.AcquiredAt
			}
		}
		return tx.Set(s.ref(), &firestoreLease{
			HolderID:    claim.HolderID,
			Description: claim.Description,
			AcquiredAt:  acquiredAt,
			ExpiresAt:   now.Add(ttl),
		})
	})
}

func (s *FirestoreLeaseStore) Renew(ctx context.Context, holderID string, now time.Time, ttl time.Duration) error {
	return s.fs.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		cur, err := s.current(tx)
		if err != nil {
			return err
		}
		if cur == nil {
			return fmt.Errorf("%w: the lease document is gone", ErrLeaseLost)
		}
		if cur.HolderID != holderID {
			return fmt.Errorf("%w: now held by %s (%s)", ErrLeaseLost, cur.Description, cur.HolderID)
		}
		// An expired lease that nobody else has claimed is still ours to renew.
		return tx.Update(s.ref(), []firestore.Update{{Path: "expires_at", Value: now.Add(ttl)}})
	})
}

func (s *FirestoreLeaseStore) Release(ctx context.Context, holderID string) error {
	return s.fs.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		cur, err := s.current(tx)
		if err != nil {
			return err
		}
		if cur == nil || cur.HolderID != holderID {
			return nil
		}
		return tx.Delete(s.ref())
	})
}

// ----- Monitor integration -----

// monitorLease is the lease configuration of a Monitor.
type monitorLease struct {
	store         LeaseStore
	claim         LeaseClaim
	ttl           time.Duration
	retryInterval time.Duration
}

// SetLease makes the monitor require the lease in store (see AcquireLease).
// A ttl of zero means defaultLeaseTTL. Without a lease the monitor runs
// unconditionally, as in unit tests that call pollers directly.
func (a *Monitor) SetLease(store LeaseStore, ttl time.Duration) {
	if ttl <= 0 {
		ttl = defaultLeaseTTL
	}
	host, _ := os.Hostname()
	a.lease = &monitorLease{
		store: store,
		claim: LeaseClaim{
			HolderID:    uuid.NewString(),
			Description: fmt.Sprintf("%s pid %d", host, os.Getpid()),
		},
		ttl:           ttl,
		retryInterval: defaultLeaseRetryInterval,
	}
}

// AcquireLease takes the lease, waiting if another holder has it. A holder
// that crashed stops blocking us once its lease expires, so we retry for one
// full ttl plus a renewal interval; a holder that is alive keeps renewing, so
// after that we give up with an error naming it. It does nothing if no lease
// is configured.
//
// When the lease is held by someone else, the first thing it does is record
// in the monitor's error log (so it shows on the dashboard, and the console)
// that this monitor is not running and why. Once it does acquire a lease it
// had to wait for, it records that the monitor is now running. A caller that
// gets an error back decides what to do about it; the error is not logged
// here.
func (a *Monitor) AcquireLease(ctx context.Context) error {
	l := a.lease
	if l == nil {
		return nil
	}
	ctx = WithFirestoreLabel(ctx, "monitor lease")
	deadline := a.clock.Now().Add(l.ttl + l.ttl/3)
	reported := false
	for {
		now := a.clock.Now()
		err := l.store.TryAcquire(ctx, l.claim, now, l.ttl)
		if err == nil {
			log.Printf("monitor lease acquired (id %s, ttl %s)", l.claim.HolderID, l.ttl)
			if reported {
				a.errorLog.Add("monitor lease acquired; the monitor is now running")
			}
			return nil
		}
		var held *LeaseHeldError
		if !errors.As(err, &held) {
			return fmt.Errorf("acquiring monitor lease: %w", err)
		}
		if !now.Before(deadline) {
			return fmt.Errorf("%w; refusing to run a second monitor against the same database", err)
		}
		if !reported {
			a.errorLog.Add("monitor is not running: %v. Waiting for that lease to expire or be released; only one monitor can run per database.", err)
			reported = true
		} else {
			log.Printf("monitor lease is held (%v); waiting for it to expire", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-a.clock.After(l.retryInterval):
		}
	}
}

// ReleaseLease gives the lease up so a replacement can start immediately
// instead of waiting for it to expire. Call it after the monitor loop has
// returned. It does nothing if no lease is configured.
func (a *Monitor) ReleaseLease(ctx context.Context) {
	if a.lease == nil {
		return
	}
	ctx = WithFirestoreLabel(ctx, "monitor lease")
	if err := a.lease.store.Release(ctx, a.lease.claim.HolderID); err != nil {
		log.Printf("releasing monitor lease: %v", err)
	}
}

// runLeaseRenewal renews the lease every ttl/3 until ctx is done, retrying
// failures every leaseFailureRetryInterval. It calls fatal and returns if the
// lease is lost, or if it hasn't been renewed for ttl*2/3 (after which we can
// no longer be sure nobody else could take it).
func (a *Monitor) runLeaseRenewal(ctx context.Context, fatal func(error)) {
	l := a.lease
	interval := l.ttl / 3
	callTimeout := min(interval, leaseCallTimeout)
	retry := min(interval, leaseFailureRetryInterval)
	lastOK := a.clock.Now()
	wait := interval
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.clock.After(wait):
		}

		callCtx, cancel := context.WithTimeout(WithFirestoreLabel(ctx, "monitor lease"), callTimeout)
		err := l.store.Renew(callCtx, l.claim.HolderID, a.clock.Now(), l.ttl)
		cancel()
		now := a.clock.Now()
		if err == nil {
			lastOK = now
			wait = interval
			continue
		}
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, ErrLeaseLost) {
			a.errorLog.Add("monitor lease: %v; stopping", err)
			fatal(err)
			return
		}
		a.errorLog.Add("monitor lease: renewal failed: %v", err)
		wait = retry
		if now.Sub(lastOK) >= l.ttl-interval {
			err = fmt.Errorf("could not renew the monitor lease for %s: %w", now.Sub(lastOK).Round(time.Second), err)
			a.errorLog.Add("monitor lease: %v; stopping", err)
			fatal(err)
			return
		}
	}
}
