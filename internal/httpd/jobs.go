package httpd

import (
	"strconv"
	"sync"
	"time"
)

// jobView is a background change to many messages, reported to the UI as
// progress: moving a sender's mail ("move"), marking a box seen ("seen") or
// trashing the mail in a box whose body matches a pattern ("filter").
type jobView struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Address  string `json:"address,omitempty"`
	Box      string `json:"box"`
	Pattern  string `json:"pattern,omitempty"`
	Total    int    `json:"total"`
	Done     int    `json:"done"`
	Moved    int    `json:"moved"`
	Finished bool   `json:"finished"`
	Error    string `json:"error,omitempty"`

	finishedAt time.Time
}

// jobKeep is how long a finished job stays listed, so a page that polls
// slowly or was reloaded still sees how it ended.
const jobKeep = 5 * time.Minute

// jobs tracks the running and recently finished background moves.
type jobs struct {
	mu   sync.Mutex
	next int
	list []*jobView
}

// start registers a job described by v and returns it with its id.
func (j *jobs) start(v jobView) jobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.next++
	v.ID = strconv.Itoa(j.next)
	j.list = append(j.list, &v)
	return v
}

// progress records how far job id has come; moved counts the messages it
// actually changed.
func (j *jobs) progress(id string, done, moved int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, v := range j.list {
		if v.ID == id {
			v.Done, v.Moved = done, moved
		}
	}
}

// finish marks job id as ended, with err when it stopped early.
func (j *jobs) finish(id string, moved int, err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, v := range j.list {
		if v.ID != id {
			continue
		}
		v.Moved, v.Finished, v.finishedAt = moved, true, time.Now()
		if err != nil {
			v.Error = err.Error()
		}
	}
}

// snapshot returns the jobs worth showing and forgets old finished ones.
func (j *jobs) snapshot() []jobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	kept := j.list[:0]
	out := make([]jobView, 0, len(j.list))
	for _, v := range j.list {
		if v.Finished && time.Since(v.finishedAt) > jobKeep {
			continue
		}
		kept = append(kept, v)
		out = append(out, *v)
	}
	j.list = kept
	return out
}
