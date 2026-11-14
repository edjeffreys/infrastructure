package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type haState struct {
	EntityID    string    `json:"entity_id"`
	State       string    `json:"state"`
	LastChanged time.Time `json:"last_changed"`
}

// importHA computes samples from HA's recorder for hours the bus never saw,
// writing one JSON sample per line for `nats pub --send-on newline`.
func importHA(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("import-ha", flag.ContinueOnError)
	haURL := fs.String("ha-url", "https://home.skaal.dev", "Home Assistant base URL")
	days := fs.Int("days", 7, "days of history to import")
	if err := fs.Parse(args); err != nil {
		return err
	}
	token := os.Getenv("HA_TOKEN")
	if token == "" {
		return errors.New("set HA_TOKEN to a long-lived access token")
	}

	now := time.Now().UTC()
	last := now.Add(-grace).Truncate(time.Hour)
	first := now.Add(-time.Duration(*days) * 24 * time.Hour).Truncate(time.Hour)
	from := first.Add(-time.Hour - lookback)

	h, err := fetchHistory(*haURL, token, from, now)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	valid := 0
	for hour := first; !hour.After(last); hour = hour.Add(time.Hour) {
		s := h.evaluate(hour)
		if s.Valid {
			valid++
		}
		if err := enc.Encode(s); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "%d hours from %s to %s, %d valid\n", int(last.Sub(first).Hours())+1, first.Format(time.RFC3339), last.Format(time.RFC3339), valid)
	return nil
}

func fetchHistory(base, token string, from, to time.Time) (history, error) {
	q := url.Values{}
	q.Set("filter_entity_id", strings.Join(entities, ","))
	q.Set("end_time", to.Format(time.RFC3339))
	q.Set("significant_changes_only", "0")
	q.Set("no_attributes", "")
	u := strings.TrimRight(base, "/") + "/api/history/period/" + url.PathEscape(from.Format(time.RFC3339)) + "?" + q.Encode()

	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("HA history: %s: %s", resp.Status, b)
	}
	return parseHistory(resp.Body)
}

func parseHistory(r io.Reader) (history, error) {
	var lists [][]haState
	if err := json.NewDecoder(r).Decode(&lists); err != nil {
		return nil, fmt.Errorf("decoding HA history: %w", err)
	}
	h := history{}
	for _, l := range lists {
		for _, st := range l {
			if tracked[st.EntityID] {
				h.get(st.EntityID).add(st.LastChanged, st.State)
			}
		}
	}
	for _, e := range entities {
		if len(h.get(e).points) == 0 {
			fmt.Fprintf(os.Stderr, "warning: no history for %s\n", e)
		}
	}
	return h, nil
}
