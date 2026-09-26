package gateway

import (
	"net/http"
	"strconv"
	"strings"
)

// PriorityHeader lets a request opt into a lower service priority by supplying
// a larger numeric value.
const PriorityHeader = "X-LLM-Priority"

const (
	PriorityMin = 0
	PriorityMax = 999
)

// EffectiveRequestPriority applies a downgrade-only header to ceiling. Lower
// numbers have higher service priority.
func EffectiveRequestPriority(r *http.Request, ceiling int) int {
	ceiling = clampPriority(ceiling)
	header := strings.TrimSpace(r.Header.Get(PriorityHeader))
	if header == "" {
		return ceiling
	}
	priority, err := strconv.Atoi(header)
	if err != nil {
		return ceiling
	}
	if priority < ceiling {
		priority = ceiling
	}
	return clampPriority(priority)
}

func clampPriority(priority int) int {
	if priority < PriorityMin {
		return PriorityMin
	}
	if priority > PriorityMax {
		return PriorityMax
	}
	return priority
}
