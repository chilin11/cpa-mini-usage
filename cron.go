package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const wakeupDefaultCron = "0 6,11,16,21 * * *"

// cronSchedule represents a standard 5-field Unix cron expression:
// minute (0-59), hour (0-23), day-of-month (1-31), month (1-12), day-of-week (0-7, 0 and 7 = Sunday).
type cronSchedule struct {
	minute  uint64
	hour    uint32
	dom     uint32
	month   uint16
	dow     uint8
	domWild bool
	dowWild bool
}

func parseCron(expr string) (cronSchedule, error) {
	normalized := strings.Join(strings.Fields(expr), " ")
	if normalized == "" || len(normalized) > 128 {
		return cronSchedule{}, errors.New("invalid cron expression")
	}
	fields := strings.Split(normalized, " ")
	if len(fields) != 5 {
		return cronSchedule{}, errors.New("cron expression must have 5 fields")
	}
	minMask, _, err := parseCronField(fields[0], 0, 59, false)
	if err != nil {
		return cronSchedule{}, err
	}
	hourMask, _, err := parseCronField(fields[1], 0, 23, false)
	if err != nil {
		return cronSchedule{}, err
	}
	domMask, domWild, err := parseCronField(fields[2], 1, 31, false)
	if err != nil {
		return cronSchedule{}, err
	}
	monthMask, _, err := parseCronField(fields[3], 1, 12, false)
	if err != nil {
		return cronSchedule{}, err
	}
	dowMask, dowWild, err := parseCronField(fields[4], 0, 7, true)
	if err != nil {
		return cronSchedule{}, err
	}
	sched := cronSchedule{
		minute:  minMask,
		hour:    uint32(hourMask),
		dom:     uint32(domMask),
		month:   uint16(monthMask),
		dow:     uint8(dowMask),
		domWild: domWild,
		dowWild: dowWild,
	}
	// Reject impossible calendar combinations such as Feb 31 ("0 0 31 2 *").
	if len(sched.nextRuns(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), 1, nil, 0)) == 0 {
		return cronSchedule{}, errors.New("cron expression never matches")
	}
	return sched, nil
}

func parseCronField(field string, minVal, maxVal int, wrapSunday bool) (uint64, bool, error) {
	if field == "" {
		return 0, false, errors.New("empty cron field")
	}
	if field == "*" {
		return cronRangeMask(minVal, maxVal, 1, wrapSunday), true, nil
	}
	var mask uint64
	parts := strings.Split(field, ",")
	for _, part := range parts {
		if part == "" {
			return 0, false, errors.New("empty cron item")
		}
		step := 1
		rangePart := part
		if slash := strings.IndexByte(part, '/'); slash >= 0 {
			if strings.IndexByte(part[slash+1:], '/') >= 0 {
				return 0, false, errors.New("invalid cron step")
			}
			s, err := parseCronInt(part[slash+1:])
			if err != nil || s <= 0 || s > maxVal-minVal+1 {
				return 0, false, errors.New("invalid cron step")
			}
			step = s
			rangePart = part[:slash]
		}
		var start, end int
		switch {
		case rangePart == "*":
			start, end = minVal, maxVal
		case strings.Contains(rangePart, "-"):
			dash := strings.IndexByte(rangePart, '-')
			if strings.IndexByte(rangePart[dash+1:], '-') >= 0 {
				return 0, false, errors.New("invalid cron range")
			}
			a, err1 := parseCronInt(rangePart[:dash])
			b, err2 := parseCronInt(rangePart[dash+1:])
			if err1 != nil || err2 != nil || a < minVal || b > maxVal || a > b {
				return 0, false, errors.New("cron range out of bounds")
			}
			start, end = a, b
		default:
			v, err := parseCronInt(rangePart)
			if err != nil || v < minVal || v > maxVal {
				return 0, false, errors.New("cron value out of bounds")
			}
			start = v
			if strings.Contains(part, "/") {
				// Support "6/5" meaning starting at 6, every 5 until field max (e.g. 6,11,16,21).
				end = maxVal
			} else {
				end = v
			}
		}
		mask |= cronRangeMask(start, end, step, wrapSunday)
	}
	if mask == 0 {
		return 0, false, errors.New("cron field matched no values")
	}
	return mask, false, nil
}

func parseCronInt(s string) (int, error) {
	if s == "" || len(s) > 2 {
		return 0, errors.New("invalid cron number")
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, errors.New("non-digit in cron number")
		}
	}
	return strconv.Atoi(s)
}

func cronRangeMask(start, end, step int, wrapSunday bool) uint64 {
	var mask uint64
	for v := start; v <= end; v += step {
		bit := v
		if wrapSunday && bit == 7 {
			bit = 0
		}
		mask |= 1 << uint(bit)
	}
	return mask
}

func (s cronSchedule) matchesMonth(m time.Month) bool {
	return (s.month & (1 << uint(m))) != 0
}

func (s cronSchedule) matchesDay(t time.Time) bool {
	domMatch := (s.dom & (1 << uint(t.Day()))) != 0
	dowMatch := (s.dow & (1 << uint(t.Weekday()))) != 0
	switch {
	case s.domWild && s.dowWild:
		return true
	case s.domWild:
		return dowMatch
	case s.dowWild:
		return domMatch
	default:
		return domMatch || dowMatch
	}
}

func (s cronSchedule) matchesHour(h int) bool {
	return (s.hour & (1 << uint(h))) != 0
}

func (s cronSchedule) matchesMinute(m int) bool {
	return (s.minute & (1 << uint(m))) != 0
}

func (s cronSchedule) matches(t time.Time) bool {
	return s.matchesMonth(t.Month()) && s.matchesDay(t) && s.matchesHour(t.Hour()) && s.matchesMinute(t.Minute())
}

func cronSlotKey(localMinute time.Time, offsetMin int) string {
	return fmt.Sprintf("cron@%s@%+d", localMinute.Format("2006-01-02T15:04"), offsetMin)
}

func (s cronSchedule) dueSlot(now time.Time, grace time.Duration, fired map[string]time.Time, offsetMin int) (string, time.Time, bool) {
	loc := wakeupZone(offsetMin)
	nowMin := now.In(loc).Truncate(time.Minute)
	steps := int(grace / time.Minute)
	if steps < 1 {
		steps = 1
	}
	for i := 0; i < steps; i++ {
		cand := nowMin.Add(-time.Duration(i) * time.Minute)
		if !s.matches(cand) {
			continue
		}
		key := cronSlotKey(cand, offsetMin)
		if _, already := fired[key]; already {
			return "", time.Time{}, false
		}
		return key, cand.UTC(), true
	}
	return "", time.Time{}, false
}

func (s cronSchedule) markRecentFired(now time.Time, grace time.Duration, fired map[string]time.Time, offsetMin int) {
	loc := wakeupZone(offsetMin)
	nowMin := now.In(loc).Truncate(time.Minute)
	steps := int(grace / time.Minute)
	if steps < 1 {
		steps = 1
	}
	for i := 0; i < steps; i++ {
		cand := nowMin.Add(-time.Duration(i) * time.Minute)
		if s.matches(cand) {
			fired[cronSlotKey(cand, offsetMin)] = now.UTC()
		}
	}
}

func (s cronSchedule) nextRuns(now time.Time, count int, fired map[string]time.Time, offsetMin int) []time.Time {
	if count <= 0 {
		return nil
	}
	loc := wakeupZone(offsetMin)
	localNow := now.In(loc)
	cur := localNow.Truncate(time.Minute)
	if cur.Before(localNow) {
		// Include current minute only if it matches and hasn't fired yet; otherwise advance.
		if !s.matches(cur) {
			cur = cur.Add(time.Minute)
		} else if fired != nil {
			if _, already := fired[cronSlotKey(cur, offsetMin)]; already {
				cur = cur.Add(time.Minute)
			}
		}
	}
	limit := cur.AddDate(4, 0, 1)
	out := make([]time.Time, 0, count)
	for cur.Before(limit) && len(out) < count {
		if !s.matchesMonth(cur.Month()) {
			cur = time.Date(cur.Year(), cur.Month()+1, 1, 0, 0, 0, 0, loc)
			continue
		}
		if !s.matchesDay(cur) {
			cur = time.Date(cur.Year(), cur.Month(), cur.Day()+1, 0, 0, 0, 0, loc)
			continue
		}
		if !s.matchesHour(cur.Hour()) {
			cur = time.Date(cur.Year(), cur.Month(), cur.Day(), cur.Hour()+1, 0, 0, 0, loc)
			continue
		}
		if !s.matchesMinute(cur.Minute()) {
			cur = cur.Add(time.Minute)
			continue
		}
		if fired != nil {
			if _, already := fired[cronSlotKey(cur, offsetMin)]; already {
				cur = cur.Add(time.Minute)
				continue
			}
		}
		out = append(out, cur.UTC())
		cur = cur.Add(time.Minute)
	}
	return out
}
