package main

import (
	"strings"
	"unicode"
)

// CPA does not expose every executor's final outbound model in usage.handle.
// These states describe name observations, never authenticity or substitution.
func modelObservationStatus(ev Event) string {
	response := observedModelName(ev.ResponseModel)
	model := observedModelName(ev.Model)
	if response == "" || model == "" {
		return "unknown"
	}
	if strings.EqualFold(model, response) {
		return "same"
	}
	if modelVariantName(model) == modelVariantName(response) {
		return "variant"
	}
	return "different"
}

// Observation fields are upstream-controlled. Reject oversized/control-bearing
// values instead of making a truncated name look like a real model identifier.
func observedModelName(name string) string {
	name = strings.TrimSpace(name)
	if len(name) > 512 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return ""
	}
	return name
}

func modelVariantName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	// CPA's (high)/(16384) route suffix is a thinking setting, not a model id.
	if i := strings.LastIndexByte(name, '('); i > 0 && strings.HasSuffix(name, ")") {
		suffix := name[i+1 : len(name)-1]
		if knownThinkingSuffix(suffix) {
			name = strings.TrimSpace(name[:i])
		}
	}
	if i := strings.LastIndexByte(name, '/'); i >= 0 && i < len(name)-1 {
		name = name[i+1:]
	}
	name = strings.TrimSuffix(name, "-latest")
	if i := strings.LastIndexByte(name, '-'); i > 0 {
		suffix := name[i+1:]
		if len(suffix) == 8 && modelDigits(suffix) {
			name = name[:i]
		} else if len(suffix) == 3 && modelDigits(suffix) {
			name = name[:i]
		}
	}
	if len(name) > 11 {
		suffix := name[len(name)-10:]
		if name[len(name)-11] == '-' && suffix[4] == '-' && suffix[7] == '-' && modelDigits(suffix[:4]) && modelDigits(suffix[5:7]) && modelDigits(suffix[8:]) {
			name = name[:len(name)-11]
		}
	}
	return strings.TrimSuffix(name, "-latest")
}

func knownThinkingSuffix(suffix string) bool {
	switch suffix {
	case "none", "auto", "minimal", "low", "medium", "high", "xhigh", "max":
		return true
	}
	return modelDigits(suffix)
}

func modelDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
