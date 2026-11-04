package app

import (
	"errors"
	"sync"
)

var (
	errOpenRegistrationUnavailable = errors.New("OPEN_REGISTRATION_UNAVAILABLE")
	errOpenRegistrationFull        = errors.New("OPEN_REGISTRATION_FULL")
	errOpenRegistrationExpired     = errors.New("OPEN_REGISTRATION_EXPIRED")
	errOpenRegistrationState       = errors.New("OPEN_REGISTRATION_STATE_CHANGED")

	openRegistrationCreateMutex sync.Mutex
	compensationDispatchMutex   sync.Mutex
)
