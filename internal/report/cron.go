package report

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Cron is a parsed standard 5-field cron expression
// (minute hour day-of-month month day-of-week).
type Cron struct {
	fields           [5]map[int]bool
	domStar, dowStar bool
}

var cronRanges = [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}

// ParseCron parses expr. Supported syntax per field: *, N, A-B, lists
// separated by commas and /step on * or ranges.
func ParseCron(expr string) (*Cron, error) {
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return nil, fmt.Errorf("cron %q: want 5 fields", expr)
	}
	c := &Cron{domStar: parts[2] == "*", dowStar: parts[4] == "*"}
	for i, p := range parts {
		set, err := parseCronField(p, cronRanges[i][0], cronRanges[i][1])
		if err != nil {
			return nil, fmt.Errorf("cron %q field %d: %w", expr, i+1, err)
		}
		if i == 4 && set[7] {
			set[0] = true // 7 is Sunday too
		}
		c.fields[i] = set
	}
	return c, nil
}

func parseCronField(s string, lo, hi int) (map[int]bool, error) {
	set := map[int]bool{}
	for _, item := range strings.Split(s, ",") {
		step := 1
		if base, st, ok := strings.Cut(item, "/"); ok {
			n, err := strconv.Atoi(st)
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("bad step %q", st)
			}
			step, item = n, base
		}
		a, b := lo, hi
		switch {
		case item == "*":
		case strings.Contains(item, "-"):
			x, y, _ := strings.Cut(item, "-")
			var err1, err2 error
			a, err1 = strconv.Atoi(x)
			b, err2 = strconv.Atoi(y)
			if err1 != nil || err2 != nil {
				return nil, fmt.Errorf("bad range %q", item)
			}
		default:
			n, err := strconv.Atoi(item)
			if err != nil {
				return nil, fmt.Errorf("bad value %q", item)
			}
			a, b = n, n
		}
		max := hi
		if hi == 6 {
			max = 7
		}
		if a < lo || b > max || a > b {
			return nil, fmt.Errorf("value out of range in %q", item)
		}
		for v := a; v <= b; v += step {
			set[v] = true
		}
	}
	return set, nil
}

func (c *Cron) match(t time.Time) bool {
	if !c.fields[0][t.Minute()] || !c.fields[1][t.Hour()] || !c.fields[3][int(t.Month())] {
		return false
	}
	dom, dow := c.fields[2][t.Day()], c.fields[4][int(t.Weekday())]
	switch {
	case c.domStar && c.dowStar:
		return true
	case c.domStar:
		return dow
	case c.dowStar:
		return dom
	default:
		return dom || dow
	}
}

// Next returns the first matching time strictly after t (in t's location).
func (c *Cron) Next(t time.Time) time.Time {
	t = t.Truncate(time.Minute).Add(time.Minute)
	for i := 0; i < 366*24*60; i++ {
		if c.match(t) {
			return t
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}
}
