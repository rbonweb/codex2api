package database

import (
	"context"
	"strings"
	"time"
)

// Near-real-time traffic snapshot: the last 5 minutes are bucketed by second. QPS and TPS
// are the average over the last 10 seconds, and each peak is the highest single second.
const (
	trafficSnapshotLookback = 5 * time.Minute
	trafficSnapshotWindow   = 10 * time.Second
)

// trafficSecondBuckets holds request and token counts per Unix second. Rows read from the
// database and entries still in the in-memory buffer are added to the same buckets.
type trafficSecondBuckets struct {
	requests map[int64]float64
	tokens   map[int64]float64
}

func newTrafficSecondBuckets() *trafficSecondBuckets {
	return &trafficSecondBuckets{requests: make(map[int64]float64), tokens: make(map[int64]float64)}
}

func (b *trafficSecondBuckets) add(sec int64, requests, tokens float64) {
	b.requests[sec] += requests
	b.tokens[sec] += tokens
}

func (b *trafficSecondBuckets) snapshot(now time.Time) *TrafficSnapshot {
	windowStart := now.Add(-trafficSnapshotWindow).Unix()
	result := &TrafficSnapshot{}
	var windowRequests, windowTokens float64
	for sec, requests := range b.requests {
		tokens := b.tokens[sec]
		result.QPSPeak = max(result.QPSPeak, requests)
		result.TPSPeak = max(result.TPSPeak, tokens)
		if sec >= windowStart {
			windowRequests += requests
			windowTokens += tokens
		}
	}
	result.QPS = windowRequests / trafficSnapshotWindow.Seconds()
	result.TPS = windowTokens / trafficSnapshotWindow.Seconds()
	return result
}

// GetTrafficSnapshot returns the near-real-time traffic snapshot.
//
// created_at records when a request finished (upstream issue #857), so requests that
// finished within the last flush interval are still in the in-memory buffer and not yet in
// the database. Reading only the database would make the last-10-second QPS swing between
// half and all of the real value with the flush cadence, so the logs in this instance's
// buffer are counted too, by completion time. With several instances, the buffers of the
// other instances are not visible here, so that part still lags by one flush interval.
func (db *DB) GetTrafficSnapshot(ctx context.Context) (*TrafficSnapshot, error) {
	now := time.Now()
	since := now.Add(-trafficSnapshotLookback)
	buckets := newTrafficSecondBuckets()
	var err error
	if db.isSQLite() {
		err = db.loadTrafficSecondsSQLite(ctx, since, buckets)
	} else {
		err = db.loadTrafficSecondsPostgres(ctx, since, buckets)
	}
	if err != nil {
		return nil, err
	}
	// Read the database first, then the buffer. A flush takes its batch out of the buffer
	// before it commits, so in this order a log is never both in the query result and still
	// in the buffer, and is never counted twice. At worst it is missed at the moment of commit.
	db.addBufferedTrafficSeconds(since, buckets)
	return buckets.snapshot(now), nil
}

func (db *DB) loadTrafficSecondsPostgres(ctx context.Context, since time.Time, buckets *trafficSecondBuckets) error {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT
			FLOOR(EXTRACT(EPOCH FROM created_at))::bigint AS sec,
			COUNT(*)::float8,
			COALESCE(SUM(total_tokens), 0)::float8
		FROM usage_logs
		WHERE created_at >= $1
		  AND TRIM(COALESCE(internal_reason, '')) = ''
		GROUP BY 1`, since)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var sec int64
		var requests, tokens float64
		if err := rows.Scan(&sec, &requests, &tokens); err != nil {
			return err
		}
		buckets.add(sec, requests, tokens)
	}
	return rows.Err()
}

// addBufferedTrafficSeconds adds the logs that are not yet written to the snapshot, by
// completion time. It counts the same rows as the database query: only rows that will be
// stored (rows that usage_log_mode drops are left out), and no internal requests.
func (db *DB) addBufferedTrafficSeconds(since time.Time, buckets *trafficSecondBuckets) {
	if db == nil {
		return
	}
	db.logMu.Lock()
	defer db.logMu.Unlock()
	for i := range db.logBuf {
		entry := &db.logBuf[i]
		if !entry.StoreUsageLog || strings.TrimSpace(entry.InternalReason) != "" || entry.CreatedAt.Before(since) {
			continue
		}
		buckets.add(entry.CreatedAt.Unix(), 1, float64(entry.TotalTokens))
	}
}
