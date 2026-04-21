// file_downloads.go
package scripts

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	_ "modernc.org/sqlite"
)

type FileDownload struct {
	Username       string  `json:"username"`
	FileName       string  `json:"file_name"`
	FilePath       string  `json:"file_path"`
	FileType       string  `json:"file_type"`
	DownloadURL    string  `json:"download_url"`
	FileSizeBytes  int64   `json:"file_size_bytes"`
	FileSizeMB     float64 `json:"file_size_mb"`
	DownloadTime   string  `json:"download_time"`
	DownloadStatus string  `json:"download_status"`
	Browser        string  `json:"browser"`
	CollectedAt    string  `json:"collected_at"`
}

// FileDownloadsScript implementa a interface Script
type FileDownloadsScript struct{}

func (f *FileDownloadsScript) Name() string {
	return "file_downloads"
}

func (f *FileDownloadsScript) Execute(args ...string) ([]map[string]interface{}, error) {
	// Define limite padrão
	limit := 50

	// Se args foram fornecidos, usa o primeiro como limit
	if len(args) > 0 {
		fmt.Sscanf(args[0], "%d", &limit)
	}

	downloads, err := f.getDownloadsSnapshot(limit)
	if err != nil {
		return []map[string]interface{}{
			{
				"username":     GetFullUsername(),
				"error":        err.Error(),
				"status":       "error",
				"collected_at": time.Now().Format("2006-01-02 15:04:05"),
			},
		}, err
	}

	// Converte []FileDownload para []map[string]interface{}
	result := make([]map[string]interface{}, 0, len(downloads))
	for _, d := range downloads {
		result = append(result, map[string]interface{}{
			"username":        d.Username,
			"file_name":       d.FileName,
			"file_path":       d.FilePath,
			"file_type":       d.FileType,
			"download_url":    d.DownloadURL,
			"file_size_bytes": d.FileSizeBytes,
			"file_size_mb":    d.FileSizeMB,
			"download_time":   d.DownloadTime,
			"download_status": d.DownloadStatus,
			"browser":         d.Browser,
			"collected_at":    d.CollectedAt,
		})
	}

	return result, nil
}

func (f *FileDownloadsScript) copyLockedFile(sourcePath, destPath string) error {
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

func (f *FileDownloadsScript) getChromeDownloads(limit int, timestamp string, tempDir string, username string) ([]FileDownload, error) {
	chromePath := filepath.Join(os.Getenv("LOCALAPPDATA"), "Google", "Chrome", "User Data", "Default", "History")

	if _, err := os.Stat(chromePath); os.IsNotExist(err) {
		return []FileDownload{}, nil
	}

	tempChrome := filepath.Join(tempDir, "chrome_downloads.db")
	if err := f.copyLockedFile(chromePath, tempChrome); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", tempChrome)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	query := `
		SELECT target_path, tab_url, total_bytes,
		       datetime(start_time/1000000-11644473600,'unixepoch') as download_time,
		       state
		FROM downloads
		ORDER BY start_time DESC
		LIMIT ?
	`

	rows, err := db.Query(query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var downloads []FileDownload

	statusMap := map[int]string{
		0: "in_progress",
		1: "completed",
		2: "cancelled",
		3: "interrupted",
		4: "dangerous",
	}

	for rows.Next() {
		var targetPath, tabURL, downloadTime string
		var totalBytes int64
		var state int

		if err := rows.Scan(&targetPath, &tabURL, &totalBytes, &downloadTime, &state); err != nil {
			continue
		}

		status := statusMap[state]
		if status == "" {
			status = "unknown"
		}

		fileName := filepath.Base(targetPath)

		downloads = append(downloads, FileDownload{
			Username:       username,
			FileName:       fileName,
			FilePath:       targetPath,
			FileType:       filepath.Ext(fileName),
			DownloadURL:    tabURL,
			FileSizeBytes:  totalBytes,
			FileSizeMB:     float64(totalBytes) / (1024 * 1024),
			DownloadTime:   downloadTime,
			DownloadStatus: status,
			Browser:        "chrome",
			CollectedAt:    timestamp,
		})
	}

	return downloads, nil
}

func (f *FileDownloadsScript) getEdgeDownloads(limit int, timestamp string, tempDir string, username string) ([]FileDownload, error) {
	edgePath := filepath.Join(os.Getenv("LOCALAPPDATA"), "Microsoft", "Edge", "User Data", "Default", "History")

	if _, err := os.Stat(edgePath); os.IsNotExist(err) {
		return []FileDownload{}, nil
	}

	tempEdge := filepath.Join(tempDir, "edge_downloads.db")
	if err := f.copyLockedFile(edgePath, tempEdge); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", tempEdge)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	query := `
		SELECT target_path, tab_url, total_bytes,
		       datetime(start_time/1000000-11644473600,'unixepoch') as download_time,
		       state
		FROM downloads
		ORDER BY start_time DESC
		LIMIT ?
	`

	rows, err := db.Query(query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var downloads []FileDownload

	statusMap := map[int]string{
		0: "in_progress",
		1: "completed",
		2: "cancelled",
		3: "interrupted",
		4: "dangerous",
	}

	for rows.Next() {
		var targetPath, tabURL, downloadTime string
		var totalBytes int64
		var state int

		if err := rows.Scan(&targetPath, &tabURL, &totalBytes, &downloadTime, &state); err != nil {
			continue
		}

		status := statusMap[state]
		if status == "" {
			status = "unknown"
		}

		fileName := filepath.Base(targetPath)

		downloads = append(downloads, FileDownload{
			Username:       username,
			FileName:       fileName,
			FilePath:       targetPath,
			FileType:       filepath.Ext(fileName),
			DownloadURL:    tabURL,
			FileSizeBytes:  totalBytes,
			FileSizeMB:     float64(totalBytes) / (1024 * 1024),
			DownloadTime:   downloadTime,
			DownloadStatus: status,
			Browser:        "edge",
			CollectedAt:    timestamp,
		})
	}

	return downloads, nil
}

func (f *FileDownloadsScript) getFirefoxDownloads(limit int, timestamp string, tempDir string, username string) ([]FileDownload, error) {
	firefoxProfilePath := filepath.Join(os.Getenv("APPDATA"), "Mozilla", "Firefox", "Profiles")

	if _, err := os.Stat(firefoxProfilePath); os.IsNotExist(err) {
		return []FileDownload{}, nil
	}

	var allDownloads []FileDownload

	profiles, err := filepath.Glob(filepath.Join(firefoxProfilePath, "*.default*"))
	if err != nil {
		return []FileDownload{}, nil
	}

	for _, profile := range profiles {
		placesDb := filepath.Join(profile, "places.sqlite")
		if _, err := os.Stat(placesDb); os.IsNotExist(err) {
			continue
		}

		tempFirefox := filepath.Join(tempDir, "firefox_"+filepath.Base(profile)+".db")
		if err := f.copyLockedFile(placesDb, tempFirefox); err != nil {
			continue
		}

		db, err := sql.Open("sqlite", tempFirefox)
		if err != nil {
			continue
		}

		query := `
			SELECT url, title, datetime(dateAdded/1000000,'unixepoch') as download_time
			FROM moz_bookmarks mb
			JOIN moz_places mp ON mb.fk = mp.id
			WHERE mb.parent IN (SELECT id FROM moz_bookmarks WHERE title = 'Downloads')
			ORDER BY dateAdded DESC
			LIMIT ?
		`

		rows, err := db.Query(query, limit)
		if err != nil {
			db.Close()
			continue
		}

		for rows.Next() {
			var url, title, downloadTime string
			if err := rows.Scan(&url, &title, &downloadTime); err != nil {
				continue
			}

			fileName := title
			if fileName == "" {
				fileName = filepath.Base(url)
			}

			allDownloads = append(allDownloads, FileDownload{
				Username:       username,
				FileName:       fileName,
				FilePath:       "",
				FileType:       filepath.Ext(fileName),
				DownloadURL:    url,
				FileSizeBytes:  0,
				FileSizeMB:     0,
				DownloadTime:   downloadTime,
				DownloadStatus: "unknown",
				Browser:        "firefox",
				CollectedAt:    timestamp,
			})
		}

		rows.Close()
		db.Close()
	}

	return allDownloads, nil
}

func (f *FileDownloadsScript) getFileSystemDownloads(limit int, timestamp string, username string) ([]FileDownload, error) {
	downloadsFolder := filepath.Join(os.Getenv("USERPROFILE"), "Downloads")

	if _, err := os.Stat(downloadsFolder); os.IsNotExist(err) {
		return []FileDownload{}, nil
	}

	var downloads []FileDownload

	thirtyDaysAgo := time.Now().AddDate(0, 0, -30)

	filepath.Walk(downloadsFolder, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}

		if info.ModTime().After(thirtyDaysAgo) {
			downloads = append(downloads, FileDownload{
				Username:       username,
				FileName:       info.Name(),
				FilePath:       path,
				FileType:       filepath.Ext(info.Name()),
				DownloadURL:    "",
				FileSizeBytes:  info.Size(),
				FileSizeMB:     float64(info.Size()) / (1024 * 1024),
				DownloadTime:   info.ModTime().Format("2006-01-02 15:04:05"),
				DownloadStatus: "completed",
				Browser:        "file_system",
				CollectedAt:    timestamp,
			})
		}

		return nil
	})

	sort.Slice(downloads, func(i, j int) bool {
		return downloads[i].DownloadTime > downloads[j].DownloadTime
	})

	if len(downloads) > limit {
		downloads = downloads[:limit]
	}

	return downloads, nil
}

func (f *FileDownloadsScript) getDownloadsSnapshot(limit int) ([]FileDownload, error) {
	timestamp := time.Now().Format("2006-01-02 15:04:05")
	username := GetFullUsername()

	tempDir := filepath.Join(os.TempDir(), "browser_dl_"+time.Now().Format("20060102_150405"))
	os.MkdirAll(tempDir, 0755)
	defer os.RemoveAll(tempDir)

	var allDownloads []FileDownload

	chrome, _ := f.getChromeDownloads(limit, timestamp, tempDir, username)
	allDownloads = append(allDownloads, chrome...)

	edge, _ := f.getEdgeDownloads(limit, timestamp, tempDir, username)
	allDownloads = append(allDownloads, edge...)

	firefox, _ := f.getFirefoxDownloads(limit, timestamp, tempDir, username)
	allDownloads = append(allDownloads, firefox...)

	fs, _ := f.getFileSystemDownloads(limit, timestamp, username)
	allDownloads = append(allDownloads, fs...)

	return allDownloads, nil
}
