package transfer

import (
	"errors"
	"os"
	"sync/atomic"
	"testing"
)

// fakeClient is a minimal RemoteClient double for exercising runTransfers'
// concurrency and reconnect-coordination logic without a real connection.
type fakeClient struct {
	concurrent bool
	downloadFn func(remotePath, localPath string) error

	connectN    atomic.Int32
	closeN      atomic.Int32
	dead        atomic.Int32 // 1 = connection dropped; Connect revives it
	inFlight    atomic.Int32
	maxInFlight atomic.Int32
}

func (f *fakeClient) Connect() error {
	f.connectN.Add(1)
	f.dead.Store(0)
	return nil
}
func (f *fakeClient) Close() error                                    { f.closeN.Add(1); return nil }
func (f *fakeClient) IsAlive() bool                                   { return f.dead.Load() == 0 }
func (f *fakeClient) Stat(path string) (*RemoteFileInfo, error)       { return nil, nil }
func (f *fakeClient) ReadDir(path string) ([]RemoteFileInfo, error)   { return nil, nil }
func (f *fakeClient) Upload(localPath, remotePath string) error       { return nil }
func (f *fakeClient) UploadResume(localPath, remotePath string) error { return nil }
func (f *fakeClient) MkdirAll(remotePath string) error                { return nil }
func (f *fakeClient) Mkdir(remotePath string) error                   { return nil }
func (f *fakeClient) Remove(remotePath string) error                  { return nil }
func (f *fakeClient) RemoveDir(remotePath string) error               { return nil }
func (f *fakeClient) Chmod(remotePath string, mode os.FileMode) error { return nil }
func (f *fakeClient) SupportsConcurrentRequests() bool                { return f.concurrent }

func (f *fakeClient) Download(remotePath, localPath string) error {
	n := f.inFlight.Add(1)
	for {
		max := f.maxInFlight.Load()
		if n <= max {
			break
		}
		if f.maxInFlight.CompareAndSwap(max, n) {
			break
		}
	}
	defer f.inFlight.Add(-1)
	if f.downloadFn != nil {
		return f.downloadFn(remotePath, localPath)
	}
	return nil
}

var (
	_ RemoteClient     = (*fakeClient)(nil)
	_ concurrentClient = (*fakeClient)(nil)
)

func makeDownloadJobs(client *fakeClient, n int) []transferJob {
	jobs := make([]transferJob, n)
	for i := range jobs {
		jobs[i] = transferJob{
			action: MirrorEntry{RelPath: "file"},
			run:    func() error { return client.Download("r", "l") },
		}
	}
	return jobs
}

func TestRunTransfers_SerialWhenNotConcurrent(t *testing.T) {
	client := &fakeClient{concurrent: false}
	jobs := makeDownloadJobs(client, 10)

	runTransfers(client, jobs, func(action MirrorEntry, err error) {
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	if max := client.maxInFlight.Load(); max != 1 {
		t.Errorf("maxInFlight = %d, want 1 (FTP-style client must stay serial)", max)
	}
}

func TestRunTransfers_ConcurrentWhenSupported(t *testing.T) {
	const numJobs = MaxConcurrentTransfers + 2

	arrived := make(chan struct{}, numJobs)
	release := make(chan struct{})

	client := &fakeClient{concurrent: true}
	client.downloadFn = func(remotePath, localPath string) error {
		arrived <- struct{}{}
		<-release
		return nil
	}

	jobs := makeDownloadJobs(client, numJobs)

	done := make(chan struct{})
	go func() {
		runTransfers(client, jobs, func(action MirrorEntry, err error) {
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
		close(done)
	}()

	// Wait until MaxConcurrentTransfers workers are simultaneously blocked
	// inside downloadFn — proves that many ran concurrently, not just that
	// the pool eventually processed every job.
	for range MaxConcurrentTransfers {
		<-arrived
	}
	close(release)
	<-done

	if max := client.maxInFlight.Load(); max < MaxConcurrentTransfers {
		t.Errorf("maxInFlight = %d, want >= %d", max, MaxConcurrentTransfers)
	}
	if max := client.maxInFlight.Load(); max > MaxConcurrentTransfers {
		t.Errorf("maxInFlight = %d, want <= %d (pool must stay bounded)", max, MaxConcurrentTransfers)
	}
}

// TestRunTransfers_SingleFlightReconnect simulates every in-flight worker's
// request failing at once (as a dropped connection would cause), and
// asserts exactly one worker actually reconnects — the rest must observe
// the coordinator's generation has already advanced and just retry.
func TestRunTransfers_SingleFlightReconnect(t *testing.T) {
	const numJobs = MaxConcurrentTransfers

	client := &fakeClient{concurrent: true}

	var attempted atomic.Int32
	failGate := make(chan struct{})

	client.downloadFn = func(remotePath, localPath string) error {
		n := attempted.Add(1)
		if n == int32(numJobs) {
			close(failGate)
		}
		<-failGate // wait until every worker has made its first attempt
		if n <= int32(numJobs) {
			client.dead.Store(1)
			return errors.New("connection lost")
		}
		return nil // retry (after reconnect) succeeds
	}

	jobs := makeDownloadJobs(client, numJobs)

	var failures int32
	runTransfers(client, jobs, func(action MirrorEntry, err error) {
		if err != nil {
			atomic.AddInt32(&failures, 1)
		}
	})

	if failures != 0 {
		t.Errorf("failures = %d, want 0 (every job should succeed after the single reconnect)", failures)
	}
	if got := client.connectN.Load(); got != 1 {
		t.Errorf("Connect() called %d times, want exactly 1", got)
	}
	if got := client.closeN.Load(); got != 1 {
		t.Errorf("Close() called %d times, want exactly 1", got)
	}
}

// A failure on a connection that is still alive (e.g. permission denied) is
// a per-file error: it must be reported as-is, without reconnecting — a
// reconnect waits out every in-flight worker, so doing it per failed file
// would stall the whole pool for nothing.
func TestRunTransfers_NoReconnectForErrorsOnLiveConnection(t *testing.T) {
	client := &fakeClient{concurrent: true}
	var attempts atomic.Int32
	client.downloadFn = func(remotePath, localPath string) error {
		attempts.Add(1)
		return errors.New("permission denied")
	}

	var failures int32
	runTransfers(client, makeDownloadJobs(client, 5), func(action MirrorEntry, err error) {
		if err != nil {
			atomic.AddInt32(&failures, 1)
		}
	})

	if failures != 5 {
		t.Errorf("failures = %d, want 5", failures)
	}
	if got := attempts.Load(); got != 5 {
		t.Errorf("attempts = %d, want 5 (no retries on a live connection)", got)
	}
	if got := client.connectN.Load(); got != 0 {
		t.Errorf("Connect() called %d times, want 0", got)
	}
}
