package master

import (
	"sync"
	"time"
)

// DeployJob represents a deployment task in the build queue
type DeployJob struct {
	TaskID        string
	GitURL        string
	ProjectName   string
	RoutingDomain string
	RoutingPort   int
	Env           map[string]string
	ChatID        int64
	MessageID     int
	CreatedAt     time.Time
	ExecuteFunc   func(job *DeployJob) error
	Dispatched    bool
}

// NodeQueue controls the active job and pending list for a specific Agent node
type NodeQueue struct {
	mu        sync.Mutex
	activeJob *DeployJob
	pending   []*DeployJob
}

// Push appends a job to the queue. Returns position (0 if active immediately) and isFirst.
func (nq *NodeQueue) Push(job *DeployJob) (int, bool) {
	nq.mu.Lock()
	defer nq.mu.Unlock()

	if nq.activeJob == nil {
		nq.activeJob = job
		return 0, true
	}

	nq.pending = append(nq.pending, job)
	return len(nq.pending), false
}

// Complete finishes the current active job and pops the next pending job if any.
// Returns the completed job, the next job, and a boolean indicating if a next job is ready to run.
func (nq *NodeQueue) Complete(taskID string) (*DeployJob, *DeployJob, bool) {
	nq.mu.Lock()
	defer nq.mu.Unlock()

	if nq.activeJob != nil && nq.activeJob.TaskID == taskID {
		completed := nq.activeJob
		nq.activeJob = nil

		if len(nq.pending) > 0 {
			next := nq.pending[0]
			nq.pending[0] = nil // Avoid memory leak
			nq.pending = nq.pending[1:]
			nq.activeJob = next
			return completed, next, true
		}
		return completed, nil, false
	}

	return nil, nil, false
}

// GetActive retrieves the current running job
func (nq *NodeQueue) GetActive() *DeployJob {
	nq.mu.Lock()
	defer nq.mu.Unlock()
	return nq.activeJob
}

// PeekPendingCount returns the number of jobs waiting in the queue
func (nq *NodeQueue) PeekPendingCount() int {
	nq.mu.Lock()
	defer nq.mu.Unlock()
	return len(nq.pending)
}

// PopNext pops and returns the next pending job without changing active state. Used during resource recovery.
func (nq *NodeQueue) PopNext() (*DeployJob, bool) {
	nq.mu.Lock()
	defer nq.mu.Unlock()

	if nq.activeJob == nil && len(nq.pending) > 0 {
		next := nq.pending[0]
		nq.pending[0] = nil // Avoid memory leak
		nq.pending = nq.pending[1:]
		nq.activeJob = next
		return next, true
	}
	return nil, false
}

// QueueCoordinator manages task queues across all agents
type QueueCoordinator struct {
	mu     sync.Mutex
	queues map[string]*NodeQueue
}

// NewQueueCoordinator initializes a QueueCoordinator
func NewQueueCoordinator() *QueueCoordinator {
	return &QueueCoordinator{
		queues: make(map[string]*NodeQueue),
	}
}

// GetQueue retrieves or initializes a NodeQueue for a given agent node
func (qc *QueueCoordinator) GetQueue(alias string) *NodeQueue {
	qc.mu.Lock()
	defer qc.mu.Unlock()

	q, exists := qc.queues[alias]
	if !exists {
		q = &NodeQueue{}
		qc.queues[alias] = q
	}
	return q
}
