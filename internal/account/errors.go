package account

import "errors"

var (
	errGithubBenefitDisabled       = errors.New("GITHUB_BENEFIT_DISABLED")
	errGithubBenefitQuotaExhausted = errors.New("GITHUB_BENEFIT_QUOTA_EXHAUSTED")
	errGithubBenefitAlreadyTG      = errors.New("GITHUB_BENEFIT_ALREADY_TG")
	errGithubBenefitAlreadyGithub  = errors.New("GITHUB_BENEFIT_ALREADY_GITHUB")
	errGithubBenefitPendingMissing = errors.New("GITHUB_BENEFIT_PENDING_MISSING")
	errGithubBenefitPendingExpired = errors.New("GITHUB_BENEFIT_PENDING_EXPIRED")
	errGithubBenefitInvalidLogin   = errors.New("GITHUB_BENEFIT_INVALID_LOGIN")
	errGithubBenefitNotOldEnough   = errors.New("GITHUB_BENEFIT_NOT_OLD_ENOUGH")
	errGithubBenefitBioMismatch    = errors.New("GITHUB_BENEFIT_BIO_MISMATCH")
)
