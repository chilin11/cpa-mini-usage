package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"time"
)

type PersistenceSettings struct {
	Enabled   bool   `json:"enabled"`
	Directory string `json:"directory"`
}

type persistedQuotas struct {
	Updated time.Time               `json:"updated"`
	Quotas  map[string]AccountQuota `json:"quotas"`
	Names   map[string]string       `json:"names"`
	Aliases map[string]string       `json:"aliases"`
}

// Called before starting workers. Older installations have no settings/quotas files.
func (e *Engine) loadPersistence() {
	e.persistence.Directory = e.dir
	body, err := os.ReadFile(filepath.Join(e.dir, "settings.json"))
	if err == nil {
		var settings struct {
			Enabled *bool `json:"persistence_enabled"`
		}
		if json.Unmarshal(body, &settings) != nil || settings.Enabled == nil {
			e.storageError = "Storage settings could not be read"
		} else {
			e.persistence.Enabled = *settings.Enabled
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		e.storageError = "Storage settings could not be read"
	}
	body, err = os.ReadFile(filepath.Join(e.dir, "quotas.json"))
	if err == nil {
		var saved persistedQuotas
		if json.Unmarshal(body, &saved) == nil {
			if saved.Quotas != nil {
				e.quotas = saved.Quotas
			}
			if saved.Names != nil {
				e.names = saved.Names
			}
			if saved.Aliases != nil {
				e.aliases = saved.Aliases
			}
			e.quotaUpdated = saved.Updated
		} else {
			e.storageError = "Quota history could not be read"
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		e.storageError = "Quota history could not be read"
	}
}

func (e *Engine) writeQuotasLocked() error {
	body, err := json.Marshal(persistedQuotas{Updated: e.quotaUpdated, Quotas: e.quotas, Names: e.names, Aliases: e.aliases})
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(e.dir, "quotas.json"), body)
}
func (e *Engine) persistQuotasLocked() {
	if e.persistence.Enabled {
		if err := e.writeQuotasLocked(); err != nil {
			e.storageError = "Quota history could not be persisted"
		}
	}
}

func (e *Engine) setPersistence(enabled bool) (PersistenceSettings, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.persistence.Directory = e.dir
	if err := os.MkdirAll(e.dir, 0700); err != nil {
		e.storageError = "Storage settings could not be persisted"
		return e.persistence, err
	}
	if enabled {
		// Flush current memory before reporting enabled; do not delete existing history when disabling.
		var records bytes.Buffer
		for _, event := range e.events {
			body, err := json.Marshal(event)
			if err != nil {
				return e.persistence, err
			}
			records.Write(body)
			records.WriteByte('\n')
		}
		if err := writeAtomic(filepath.Join(e.dir, "events.jsonl"), records.Bytes()); err != nil {
			e.storageError = "Unable to persist request statistics"
			return e.persistence, err
		}
		if err := e.writeQuotasLocked(); err != nil {
			e.storageError = "Quota history could not be persisted"
			return e.persistence, err
		}
		prices, err := json.Marshal(map[string]any{"updated": e.priceUpdated, "prices": e.prices})
		if err != nil {
			return e.persistence, err
		}
		if err = writeAtomic(filepath.Join(e.dir, "prices.json"), prices); err != nil {
			e.storageError = "Price catalog could not be persisted"
			return e.persistence, err
		}
	}
	settings, _ := json.Marshal(map[string]bool{"persistence_enabled": enabled})
	if err := writeAtomic(filepath.Join(e.dir, "settings.json"), settings); err != nil {
		e.storageError = "Storage settings could not be persisted"
		return e.persistence, err
	}
	e.persistence.Enabled = enabled
	e.storageError = ""
	return e.persistence, nil
}

// encoding/json serializes time.Duration as nanoseconds, not milliseconds or seconds.
func durationMilliseconds(record map[string]any, keys ...string) *float64 {
	value := getAny(record, keys...)
	var nanos float64
	switch x := value.(type) {
	case float64:
		nanos = x
	case json.Number:
		var err error
		nanos, err = x.Float64()
		if err != nil {
			return nil
		}
	default:
		return nil
	}
	if math.IsNaN(nanos) || math.IsInf(nanos, 0) || nanos <= 0 || nanos >= float64(math.MaxInt64) {
		return nil
	}
	return floatPtr(nanos / float64(time.Millisecond))
}
