package books

import (
	"fmt"
	"log"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"ccoff-abs-bot/internal/abs"
	"ccoff-abs-bot/internal/model"
	"ccoff-abs-bot/internal/storage"
	"ccoff-abs-bot/internal/textfmt"
)

const (
	bookLibraryEventLookback     = 72 * time.Hour
	bookLibraryEventCollectEvery = 5 * time.Minute
	bookLibraryEventScanLimit    = 5
)

var (
	bookLibraryEventMu       sync.Mutex
	bookLibraryEventLastScan time.Time
)

// StartBookLibraryEventCollector runs a background loop that periodically scans
// the ABS libraries and persists recent additions locally.
func StartBookLibraryEventCollector() {
	go func() {
		runBookLibraryEventScanSafely()
		ticker := time.NewTicker(bookLibraryEventCollectEvery)
		defer ticker.Stop()
		for range ticker.C {
			runBookLibraryEventScanSafely()
		}
	}()
	log.Println("✅ 求书入库事件采集器已启动：每 5 分钟持久化最近 ABS 新增书籍。")
}

func runBookLibraryEventScanSafely() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("⚠️ 求书入库事件采集 panic，已恢复: panic=%s", textfmt.FormatPlainValue(r))
		}
	}()
	if !bookLibraryEventMu.TryLock() {
		return
	}
	defer bookLibraryEventMu.Unlock()
	if !bookLibraryEventLastScan.IsZero() && time.Since(bookLibraryEventLastScan) < bookLibraryEventCollectEvery/2 {
		return
	}
	bookLibraryEventLastScan = time.Now()
	if err := scanAndPersistBookLibraryEvents(time.Now()); err != nil {
		log.Printf("⚠️ 求书入库事件采集失败: %s", textfmt.FormatPlainError(err))
	}
}

func scanAndPersistBookLibraryEvents(now time.Time) error {
	if storage.DB == nil || abs.DefaultClient == nil {
		return fmt.Errorf("BOOK_LIBRARY_EVENT_NOT_READY")
	}
	if now.IsZero() {
		now = time.Now()
	}

	libraries, err := getAbsLibraries(abs.DefaultClient)
	if err != nil {
		return err
	}
	if len(libraries) == 0 {
		return nil
	}

	libraryNames := make(map[string]string, len(libraries))
	for _, library := range libraries {
		libraryNames[strings.TrimSpace(library.ID)] = strings.TrimSpace(library.Name)
	}

	events := make([]model.BookLibraryEvent, 0, 64)
	for _, library := range libraries {
		libraryID := strings.TrimSpace(library.ID)
		if libraryID == "" {
			continue
		}
		// 只读取每个媒体库按 addedAt 倒序的最新 5 条，足够覆盖近期新增且不会给 ABS 造成压力。
		items, pageErr := getRecentAbsLibraryItemsPage(abs.DefaultClient, libraryID, bookLibraryEventScanLimit, 0)
		if pageErr != nil {
			log.Printf("求书入库事件 ABS 最新条目读取失败: library=%s err=%s", textfmt.FormatPlainValue(libraryID), textfmt.FormatPlainError(pageErr))
		} else {
			for _, item := range items {
				candidate := bookAnnouncementCandidateFromItem(item, libraryNames)
				if candidate.ItemID == "" || candidate.RecentAt.IsZero() {
					continue
				}
				if candidate.RecentAt.Before(now.Add(-bookLibraryEventLookback)) {
					continue
				}
				events = append(events, model.BookLibraryEvent{
					ItemID:      candidate.ItemID,
					LibraryID:   candidate.LibraryID,
					LibraryName: candidate.LibraryName,
					Title:       candidate.Title,
					Narrators:   candidate.Narrators,
					RecentAt:    candidate.RecentAt,
					Source:      "scan",
					FirstSeenAt: now,
				})
			}
		}
	}

	if len(events) == 0 {
		return nil
	}

	if err := storage.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "item_id"}},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"library_id":   gorm.Expr("excluded.library_id"),
			"library_name": gorm.Expr("excluded.library_name"),
			"title":        gorm.Expr("excluded.title"),
			"narrators":    gorm.Expr("excluded.narrators"),
			"recent_at":    gorm.Expr("excluded.recent_at"),
			"source":       gorm.Expr("excluded.source"),
			// Preserve the first time the event was seen locally.
			"updated_at": gorm.Expr("excluded.updated_at"),
		}),
	}).CreateInBatches(&events, 50).Error; err != nil {
		return err
	}

	// 清理太久以前的事件，避免表无限增长。
	cutoff := now.Add(-bookLibraryEventLookback)
	if err := storage.DB.Where("recent_at < ?", cutoff).Delete(&model.BookLibraryEvent{}).Error; err != nil {
		log.Printf("⚠️ 清理过期求书入库事件失败: %s", textfmt.FormatPlainError(err))
	}
	return nil
}

func loadRecentBookLibraryEvents(limit int) ([]model.BookLibraryEvent, error) {
	if storage.DB == nil {
		return nil, fmt.Errorf("BOOK_LIBRARY_EVENT_DB_EMPTY")
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	var events []model.BookLibraryEvent
	err := storage.DB.Where("recent_at >= ?", time.Now().Add(-bookLibraryEventLookback)).
		Order("recent_at DESC, id DESC").
		Limit(limit).
		Find(&events).Error
	return events, err
}

func bookLibraryEventsAsCandidates(events []model.BookLibraryEvent) []BookAnnouncementCandidate {
	candidates := make([]BookAnnouncementCandidate, 0, len(events))
	for _, event := range events {
		candidate := BookAnnouncementCandidate{
			ItemID:      event.ItemID,
			LibraryID:   event.LibraryID,
			LibraryName: event.LibraryName,
			Title:       event.Title,
			Narrators:   event.Narrators,
			RecentAt:    event.RecentAt,
		}
		if candidate.ItemID == "" || candidate.RecentAt.IsZero() {
			continue
		}
		candidates = append(candidates, candidate)
	}
	return candidates
}

// extractXmlyTitleHint attempts to extract a usable book-title hint from a
// Ximalaya URL. It is deliberately conservative: if no useful text can be
// found it returns "". The main use is to help rank stored ABS events, not to
// be treated as authoritative metadata.
func extractXmlyTitleHint(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if host != "ximalaya.com" && host != "www.ximalaya.com" && host != "m.ximalaya.com" && host != "xima.tv" {
		return ""
	}
	if q := strings.TrimSpace(u.Query().Get("title")); q != "" {
		return q
	}
	parts := strings.FieldsFunc(strings.Trim(u.Path, "/"), func(r rune) bool { return r == '/' })
	knownTypes := map[string]bool{
		"album": true, "albums": true, "sound": true, "sounds": true,
		"xi": true, "ximalaya": true, "mobile": true, "m": true,
	}
	for i := len(parts) - 1; i >= 0; i-- {
		part := strings.TrimSpace(parts[i])
		if part == "" {
			continue
		}
		// Numeric-only segments are IDs, not useful title hints.
		if _, err := strconv.Atoi(part); err == nil {
			continue
		}
		if knownTypes[strings.ToLower(part)] {
			continue
		}
		if len([]rune(part)) >= 2 {
			return part
		}
	}
	return ""
}

func normalizeBookTitleForMatch(s string) string {
	s = strings.TrimSpace(textfmt.FormatDiagnosticTextForDisplay(s))
	s = strings.ToLower(s)
	// Keep Chinese/letters/numbers; collapse separators and spaces for loose matching.
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func bookTitleMatchesHint(bookTitle string, hint string) bool {
	a := normalizeBookTitleForMatch(bookTitle)
	b := normalizeBookTitleForMatch(hint)
	if a == "" || b == "" {
		return false
	}
	return strings.Contains(a, b) || strings.Contains(b, a)
}

func sortBookAnnouncementCandidatesByRequestHint(candidates []BookAnnouncementCandidate, hint string) {
	if hint == "" || len(candidates) <= 1 {
		return
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		li := bookTitleMatchesHint(candidates[i].Title, hint)
		lj := bookTitleMatchesHint(candidates[j].Title, hint)
		if li != lj {
			return li
		}
		return candidates[i].RecentAt.After(candidates[j].RecentAt)
	})
}
