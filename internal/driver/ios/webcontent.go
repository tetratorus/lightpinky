package ios

import (
	"context"
	"path"
	"slices"
	"strings"
	"time"
)

// isWebContent reports whether an executable is WebKit's web content
// process, which renders every page of Safari and of the apps that show
// one (com.apple.WebKit.WebContent, and its EnhancedSecurity variant).
func isWebContent(exe string) bool {
	return strings.HasPrefix(path.Base(exe), "com.apple.WebKit.WebContent")
}

// webTTL is how long a process stays classified: a process id can be
// given to another process once the first one ends.
const webTTL = 30 * time.Second

// webCache remembers, by process id, which processes are web content.
type webCache struct {
	m map[int]webEntry
}

type webEntry struct {
	web bool
	at  time.Time
}

// stale returns the process ids that have no classification, or one older
// than webTTL.
func (c *webCache) stale(pids []int, now time.Time) []int {
	var out []int
	for _, pid := range pids {
		if e, ok := c.m[pid]; (!ok || now.Sub(e.at) >= webTTL) && !slices.Contains(out, pid) {
			out = append(out, pid)
		}
	}
	return out
}

// learn classifies the processes of a walk from a process list (a process
// that is not listed loses its classification) and drops the old ones. It
// takes every process of the walk, not only the stale ones: reading the
// list can last past the time another one expires.
func (c *webCache) learn(pids []int, procs map[uint64]string, now time.Time) {
	if c.m == nil {
		c.m = map[int]webEntry{}
	}
	for pid, e := range c.m {
		if now.Sub(e.at) >= webTTL {
			delete(c.m, pid)
		}
	}
	for _, pid := range pids {
		if exe, ok := procs[uint64(pid)]; ok {
			c.m[pid] = webEntry{isWebContent(exe), now}
		} else {
			delete(c.m, pid)
		}
	}
}

// markWebContent returns els with Web set on the elements of web content
// processes. A page's elements are listed beside the app's own, from a
// process of their own, so the process list tells them apart from other
// processes with no host (widgets). When the list cannot be read, the
// classification kept so far stands.
func (d *Device) markWebContent(ctx context.Context, els []raxElement) []raxElement {
	appPID := els[0].Object.PID
	var pids []int
	for _, e := range els {
		if e.Object.PID != appPID {
			pids = append(pids, e.Object.PID)
		}
	}
	d.svcMu.Lock()
	stale := d.web.stale(pids, time.Now())
	d.svcMu.Unlock()
	if len(stale) > 0 {
		if procs, err := d.processPaths(ctx); err == nil {
			d.svcMu.Lock()
			d.web.learn(pids, procs, time.Now())
			d.svcMu.Unlock()
		}
	}
	out := slices.Clone(els) // els may be a pushed tree, which others read
	d.svcMu.Lock()
	defer d.svcMu.Unlock()
	for i := range out {
		out[i].Web = d.web.m[out[i].Object.PID].web
	}
	return out
}
