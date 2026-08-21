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

	connectN    int32
	closeN      int32
	inFlight    int32
	maxInFlight int32
}

func (f *fakeClient) Connect() error                                  { atomic.AddInt32(&f.connectN, 1); return nil }
func (f *fakeClient) Close() error                                    { atomic.AddInt32(&f.closeN, 1); return nil }
func (f *fakeClient) IsAlive() bool                                   { return true }
func (f *fakeClient) Stat(path string) (*RemoteFileInfo, error)       { return nil, nil }
func (f *fakeClient) ReadDir(path string) ([]RemoteFileInfo, error)   { return nil, nil }
func (f *fakeClient) Upload(localPath, remotePath string) error       { return nil }
func (f *fakeClient) UploadResume(localPath, remotePath string) error { return nil }
func (f *fakeClient) MkdirAll(remotePath string) error                { return nil }
func (f *fakeClient) Remove(remotePath string) error                  { return nil }
func (f *fakeClient) RemoveDir(remotePath string) error               { return nil }
func (f *fakeClient) Chmod(remotePath string, mode os.FileMode) error { return nil }
func (f *fakeClient) SupportsConcurrentRequests() bool                { return f.concurrent }

func (f *fakeClient) Download(remotePath, localPath string) error {
	n := atomic.AddInt32(&f.inFlight, 1)
	for {
		max := atomic.LoadInt32(&f.maxInFlight)
		if n <= max {
			break
		}
		if atomic.CompareAndSwapInt32(&f.maxInFlight, max, n) {
			break
		}
	}
	defer atomic.AddInt32(&f.inFlight, -1)
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

	if max := atomic.LoadInt32(&client.maxInFlight); max != 1 {
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
	for i := 0; i < MaxConcurrentTransfers; i++ {
		<-arrived
	}
	close(release)
	<-done

	if max := atomic.LoadInt32(&client.maxInFlight); max < MaxConcurrentTransfers {
		t.Errorf("maxInFlight = %d, want >= %d", max, MaxConcurrentTransfers)
	}
	if max := atomic.LoadInt32(&client.maxInFlight); max > MaxConcurrentTransfers {
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

	var attempted int32
	failGate := make(chan struct{})

	client.downloadFn = func(remotePath, localPath string) error {
		n := atomic.AddInt32(&attempted, 1)
		if n == int32(numJobs) {
			close(failGate)
		}
		<-failGate // wait until every worker has made its first attempt
		if n <= int32(numJobs) {
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
	if got := atomic.LoadInt32(&client.connectN); got != 1 {
		t.Errorf("Connect() called %d times, want exactly 1", got)
	}
	if got := atomic.LoadInt32(&client.closeN); got != 1 {
		t.Errorf("Close() called %d times, want exactly 1", got)
	}
}
