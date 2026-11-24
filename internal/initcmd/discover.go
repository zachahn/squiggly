package initcmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/zachahn/squiggly/internal/config"
)

const discoveryTimeout = 15 * time.Second

func DiscoverModels(ctx context.Context, client *http.Client, platform, baseURL, apiKey string) ([]string, error) {
	root := strings.TrimRight(baseURL, "/")
	var url string
	if platform == config.PlatformOllama {
		url = root + "/api/tags"
	} else {
		url = root + "/models"
	}

	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %s: %w", url, err)
	}
	if platform != config.PlatformOllama && apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("read response from %s: %w", url, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("GET %s returned %s", url, resp.Status)
	}

	var models []string
	if platform == config.PlatformOllama {
		var parsed struct {
			Models []struct {
				Name  string `json:"name"`
				Model string `json:"model"`
			} `json:"models"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, fmt.Errorf("parse response from %s: %w", url, err)
		}
		for _, m := range parsed.Models {
			if m.Name != "" {
				models = append(models, m.Name)
			} else if m.Model != "" {
				models = append(models, m.Model)
			}
		}
	} else {
		var parsed struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, fmt.Errorf("parse response from %s: %w", url, err)
		}
		for _, m := range parsed.Data {
			if m.ID != "" {
				models = append(models, m.ID)
			}
		}
	}
	return models, nil
}
