// Package providermodels lists the models a provider serves through its
// OpenAI-compatible model listing endpoint (GET .../v1/models). It backs
// the console's model pickers; dispatch never depends on it.
package providermodels

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// maxBody bounds the listing response.
const maxBody = 1 << 20

// ErrUnavailable wraps every failure to obtain a listing (transport,
// status, decoding); the cause never carries the credential.
var ErrUnavailable = errors.New("providermodels: model listing unavailable")

// List fetches url (the provider's models endpoint) with an optional
// bearer token and returns the model ids, de-duplicated and sorted. Both
// the OpenAI shape ({"data":[{"id":…}]}) and the Ollama shape
// ({"models":[{"name":…}]}) are understood.
func List(ctx context.Context, client *http.Client, url, token string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d", ErrUnavailable, resp.StatusCode)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&body); err != nil {
		return nil, fmt.Errorf("%w: decode: %v", ErrUnavailable, err)
	}
	seen := map[string]bool{}
	out := []string{}
	add := func(id string) {
		if id = strings.TrimSpace(id); id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, d := range body.Data {
		add(d.ID)
	}
	if len(out) == 0 {
		for _, m := range body.Models {
			if m.Model != "" {
				add(m.Model)
			} else {
				add(m.Name)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}
