package datastore

import (
	"container/list"
	"expvar"
	"sync"
	"time"

	"github.com/jhillyerd/inbucket/config"
	"github.com/jhillyerd/inbucket/log"
)

var (
	retentionScanCompleted   = time.Now()
	retentionScanCompletedMu sync.RWMutex

	// History counters
	expRetentionDeletesTotal = new(expvar.Int)
	expRetentionPeriod       = new(expvar.Int)
	expRetainedCurrent       = new(expvar.Int)

	// History of certain stats
	retentionDeletesHist = list.New()
	retainedHist         = list.New()

	// History rendered as comma delimited string
	expRetentionDeletesHist = new(expvar.String)
	expRetainedHist         = new(expvar.String)
)

func init() {
	rm := expvar.NewMap("retention")
	rm.Set("SecondsSinceScanCompleted", expvar.Func(secondsSinceRetentionScanCompleted))
	rm.Set("DeletesHist", expRetentionDeletesHist)
	rm.Set("DeletesTotal", expRetentionDeletesTotal)
	rm.Set("Period", expRetentionPeriod)
	rm.Set("RetainedHist", expRetainedHist)
	rm.Set("RetainedCurrent", expRetainedCurrent)

	log.AddTickerFunc(func() {
		expRetentionDeletesHist.Set(log.PushMetric(retentionDeletesHist, expRetentionDeletesTotal))
		expRetainedHist.Set(log.PushMetric(retainedHist, expRetainedCurrent))
	})
}

// RetentionScanner looks for messages older than the configured retention period and deletes them.
type RetentionScanner struct {
	globalShutdown    chan bool // Closes when Inbucket needs to shut down
	retentionShutdown chan bool // Closed after the scanner has shut down
	ds                DataStore
	retentionPeriod   time.Duration
	retentionSleep    time.Duration
}

// NewRetentionScanner launches a go-routine that scans for expired
// messages, following the configured interval
func NewRetentionScanner(ds DataStore, shutdownChannel chan bool) *RetentionScanner {
	cfg := config.GetDataStoreConfig()
	rs := &RetentionScanner{
		globalShutdown:    shutdownChannel,
		retentionShutdown: make(chan bool),
		ds:                ds,
		retentionPeriod:   time.Duration(cfg.RetentionMinutes) * time.Minute,
		retentionSleep:    time.Duration(cfg.RetentionSleep) * time.Millisecond,
	}
	// expRetentionPeriod is displayed on the status page
	expRetentionPeriod.Set(int64(cfg.RetentionMinutes * 60))
	return rs
}

// Start up the retention scanner if retention period > 0
func (rs *RetentionScanner) Start() {
	if rs.retentionPeriod <= 0 {
		log.Infof("Retention scanner disabled")
		close(rs.retentionShutdown)
		return
	}
	log.Infof("Retention configured for %v", rs.retentionPeriod)
	go rs.run()
}

// run loops to kick off the scanner on the correct schedule
func (rs *RetentionScanner) run() {
	start := time.Now()
retentionLoop:
	for {
		// Prevent scanner from starting more than once a minute
		since := time.Since(start)
		if since < time.Minute {
			dur := time.Minute - since
			log.Tracef("Retention scanner sleeping for %v", dur)
			select {
			case <-rs.globalShutdown:
				break retentionLoop
			case <-time.After(dur):
			}
		}
		// Kickoff scan
		start = time.Now()
		if err := rs.doScan(); err != nil {
			log.Errorf("Error during retention scan: %v", err)
		}
		// Check for global shutdown
		select {
		case <-rs.globalShutdown:
			break retentionLoop
		default:
		}
	}
	log.Tracef("Retention scanner shut down")
	close(rs.retentionShutdown)
}

// doScan does a single pass of all mailboxes looking for messages that can be purged
func (rs *RetentionScanner) doScan() error {
	log.Tracef("Starting retention scan")
	cutoff := time.Now().Add(-1 * rs.retentionPeriod)
	mboxes, err := rs.ds.AllMailboxes()
	if err != nil {
		return err
	}
	retained := 0
	// Loop over all mailboxes
	for _, mb := range mboxes {
		messages, err := mb.GetMessages()
		if err != nil {
			return err
		}
		// Loop over all messages in mailbox
		for _, msg := range messages {
			if msg.Date().Before(cutoff) {
				log.Tracef("Purging expired message %v", msg.ID())
				err = msg.Delete()
				if err != nil {
					// Log but don't abort
					log.Errorf("Failed to purge message %v: %v", msg.ID(), err)
				} else {
					expRetentionDeletesTotal.Add(1)
				}
			} else {
				retained++
			}
		}
		// Sleep after completing a mailbox
		select {
		case <-rs.globalShutdown:
			log.Tracef("Retention scan aborted due to shutdown")
			return nil
		case <-time.After(rs.retentionSleep):
			// Reduce disk thrashing
		}
	}
	// Update metrics
	setRetentionScanCompleted(time.Now())
	expRetainedCurrent.Set(int64(retained))
	return nil
}

// Join does not retun until the retention scanner has shut down
func (rs *RetentionScanner) Join() {
	if rs.retentionShutdown != nil {
		<-rs.retentionShutdown
	}
}

func setRetentionScanCompleted(t time.Time) {
	retentionScanCompletedMu.Lock()
	defer retentionScanCompletedMu.Unlock()
	retentionScanCompleted = t
}

func getRetentionScanCompleted() time.Time {
	retentionScanCompletedMu.RLock()
	defer retentionScanCompletedMu.RUnlock()
	return retentionScanCompleted
}

func secondsSinceRetentionScanCompleted() interface{} {
	return time.Since(getRetentionScanCompleted()) / time.Second
}
