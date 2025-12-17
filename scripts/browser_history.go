// browser_history.go
package scripts

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type BrowserHistory struct {
	Browser     string `json:"browser"`
	URL         string `json:"url"`
	Title       string `json:"title"`
	LastVisit   string `json:"last_visit"`
	CollectedAt string `json:"collected_at"`
}

// BrowserHistoryScript implementa a interface Script
type BrowserHistoryScript struct{}

func (b *BrowserHistoryScript) Name() string {
	return "browser_history"
}

func (b *BrowserHistoryScript) Execute(args ...string) ([]map[string]interface{}, error) {
	// Define limite padrão
	limit := 100

	// Se args foram fornecidos, usa o primeiro como limit
	if len(args) > 0 {
		fmt.Sscanf(args[0], "%d", &limit)
	}

	history, err := b.getBrowserHistorySnapshot(limit)

	if err != nil {
		return []map[string]interface{}{
			{
				"browser":      "error",
				"error":        err.Error(),
				"collected_at": time.Now().Format("2006-01-02 15:04:05"),
			},
		}, err
	}

	// Filtra histórico sem título
	var filteredHistory []BrowserHistory
	for _, h := range history {
		if strings.TrimSpace(h.Title) != "" {
			filteredHistory = append(filteredHistory, h)
		}
	}

	if len(filteredHistory) == 0 {
		return []map[string]interface{}{}, nil
	}

	// Converte []BrowserHistory para []map[string]interface{}
	result := make([]map[string]interface{}, 0, len(filteredHistory))
	for _, h := range filteredHistory {
		result = append(result, map[string]interface{}{
			"browser":      h.Browser,
			"url":          h.URL,
			"title":        h.Title,
			"last_visit":   h.LastVisit,
			"collected_at": h.CollectedAt,
		})
	}

	return result, nil
}

func (b *BrowserHistoryScript) copyLockedFile(sourcePath, destPath string) error {
	source, err := os.OpenFile(sourcePath, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer source.Close()

	dest, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer dest.Close()

	_, err = io.Copy(dest, source)
	return err
}

func (b *BrowserHistoryScript) getChromeHistory(limit int, timestamp string, tempDir string) ([]BrowserHistory, error) {
	chromePath := filepath.Join(os.Getenv("LOCALAPPDATA"), "Google", "Chrome", "User Data", "Default", "History")

	if _, err := os.Stat(chromePath); os.IsNotExist(err) {
		return []BrowserHistory{}, nil
	}

	tempChrome := filepath.Join(tempDir, "chrome_history.db")
	if err := b.copyLockedFile(chromePath, tempChrome); err != nil {
		return nil, fmt.Errorf("erro ao copiar Chrome history: %v", err)
	}

	db, err := sql.Open("sqlite", tempChrome)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	query := fmt.Sprintf(`
		SELECT url, title, datetime(last_visit_time/1000000-11644473600,'unixepoch') as last_visit
		FROM urls
		WHERE title IS NOT NULL AND title != ''
		ORDER BY last_visit_time DESC
		LIMIT %d
	`, limit)

	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var history []BrowserHistory

	for rows.Next() {
		var url, title, lastVisit string
		if err := rows.Scan(&url, &title, &lastVisit); err != nil {
			continue
		}
		history = append(history, BrowserHistory{
			Browser:     "Chrome",
			URL:         url,
			Title:       title,
			LastVisit:   lastVisit,
			CollectedAt: timestamp,
		})
	}

	return history, nil
}

func (b *BrowserHistoryScript) getEdgeHistory(limit int, timestamp string, tempDir string) ([]BrowserHistory, error) {
	edgePath := filepath.Join(os.Getenv("LOCALAPPDATA"), "Microsoft", "Edge", "User Data", "Default", "History")

	if _, err := os.Stat(edgePath); os.IsNotExist(err) {
		return []BrowserHistory{}, nil
	}

	tempEdge := filepath.Join(tempDir, "edge_history.db")
	if err := b.copyLockedFile(edgePath, tempEdge); err != nil {
		return nil, fmt.Errorf("erro ao copiar Edge history: %v", err)
	}

	db, err := sql.Open("sqlite", tempEdge)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	query := fmt.Sprintf(`
		SELECT url, title, datetime(last_visit_time/1000000-11644473600,'unixepoch') as last_visit
		FROM urls
		WHERE title IS NOT NULL AND title != ''
		ORDER BY last_visit_time DESC
		LIMIT %d
	`, limit)

	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var history []BrowserHistory

	for rows.Next() {
		var url, title, lastVisit string
		if err := rows.Scan(&url, &title, &lastVisit); err != nil {
			continue
		}
		history = append(history, BrowserHistory{
			Browser:     "Edge",
			URL:         url,
			Title:       title,
			LastVisit:   lastVisit,
			CollectedAt: timestamp,
		})
	}

	return history, nil
}

func (b *BrowserHistoryScript) getFirefoxHistory(limit int, timestamp string, tempDir string) ([]BrowserHistory, error) {
	firefoxProfilePath := filepath.Join(os.Getenv("APPDATA"), "Mozilla", "Firefox", "Profiles")

	if _, err := os.Stat(firefoxProfilePath); os.IsNotExist(err) {
		return []BrowserHistory{}, nil
	}

	profiles, err := filepath.Glob(filepath.Join(firefoxProfilePath, "*.default*"))
	if err != nil {
		return []BrowserHistory{}, nil
	}

	var allHistory []BrowserHistory

	for _, profile := range profiles {
		placesDb := filepath.Join(profile, "places.sqlite")

		if _, err := os.Stat(placesDb); os.IsNotExist(err) {
			continue
		}

		profileName := filepath.Base(profile)
		tempFirefox := filepath.Join(tempDir, fmt.Sprintf("firefox_history_%s.db", profileName))

		if err := b.copyLockedFile(placesDb, tempFirefox); err != nil {
			continue
		}

		db, err := sql.Open("sqlite", tempFirefox)
		if err != nil {
			continue
		}

		query := fmt.Sprintf(`
			SELECT url, title, datetime(last_visit_date/1000000,'unixepoch') as last_visit
			FROM moz_places
			WHERE title IS NOT NULL AND title != ''
			ORDER BY last_visit_date DESC
			LIMIT %d
		`, limit)

		rows, err := db.Query(query)
		if err != nil {
			db.Close()
			continue
		}

		for rows.Next() {
			var url, title, lastVisit string
			if err := rows.Scan(&url, &title, &lastVisit); err != nil {
				continue
			}
			allHistory = append(allHistory, BrowserHistory{
				Browser:     "Firefox",
				URL:         url,
				Title:       title,
				LastVisit:   lastVisit,
				CollectedAt: timestamp,
			})
		}

		rows.Close()
		db.Close()
	}

	return allHistory, nil
}

func (b *BrowserHistoryScript) getBrowserHistorySnapshot(limit int) ([]BrowserHistory, error) {
	timestamp := time.Now().Format("2006-01-02 15:04:05")

	tempDir := filepath.Join(os.TempDir(), fmt.Sprintf("browser_history_%s", time.Now().Format("20060102_150405")))

	if err := os.MkdirAll(tempDir, 0755); err != nil {
		return nil, fmt.Errorf("erro ao criar diretório temporário: %v", err)
	}
	defer os.RemoveAll(tempDir)

	var allHistory []BrowserHistory

	chromeHistory, _ := b.getChromeHistory(limit, timestamp, tempDir)
	allHistory = append(allHistory, chromeHistory...)

	edgeHistory, _ := b.getEdgeHistory(limit, timestamp, tempDir)
	allHistory = append(allHistory, edgeHistory...)

	firefoxHistory, _ := b.getFirefoxHistory(limit, timestamp, tempDir)
	allHistory = append(allHistory, firefoxHistory...)

	return allHistory, nil
}
