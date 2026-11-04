package books

import "errors"

var (
	errBookAnnouncementAlreadySent = errors.New("BOOK_ANNOUNCEMENT_ALREADY_SENT")
	errBookAnnouncementInProgress  = errors.New("BOOK_ANNOUNCEMENT_IN_PROGRESS")
	errBookAnnouncementUncertain   = errors.New("BOOK_ANNOUNCEMENT_UNCERTAIN")
)
