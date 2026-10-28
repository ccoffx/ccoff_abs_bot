package storage

import (
	"errors"
	"strings"
)

const unknownBusinessErrorCode = "UNKNOWN"

var (
	ErrLotteryNotActive               = errors.New("LOTTERY_NOT_ACTIVE")
	ErrLotteryWaitingDraw             = errors.New("LOTTERY_WAITING_DRAW")
	ErrLotteryFull                    = errors.New("LOTTERY_FULL")
	ErrLotteryAlreadyJoined           = errors.New("ALREADY_JOINED")
	ErrLotteryClaimExpired            = errors.New("LOTTERY_CLAIM_EXPIRED")
	ErrMarketplaceCloseNotFound       = errors.New("MARKETPLACE_CLOSE_NOT_FOUND")
	ErrSectSecretRealmNotActive       = errors.New("REALM_NOT_ACTIVE")
	ErrSectSecretRealmAlreadyActive   = errors.New("REALM_ALREADY_ACTIVE")
	ErrSectSecretRealmWeeklyLimit     = errors.New("REALM_WEEKLY_LIMIT")
	ErrSectTechnologyLevelChanged     = errors.New("TECH_LEVEL_CHANGED")
	ErrPointsNotEnough                = errors.New("POINTS_NOT_ENOUGH")
	ErrInsufficientPoints             = errors.New("INSUFFICIENT_POINTS")
	ErrAlreadyGrabbed                 = errors.New("ALREADY_GRABBED")
	ErrConcurrentRedPacketGrabRetry   = errors.New("CONCURRENT_REDPACKET_GRAB_RETRY")
	errAlreadyBet                     = errors.New("ALREADY_BET")
	ErrUsageLimitReached              = errors.New("USAGE_LIMIT_REACHED")
	ErrItemNotEnough                  = errors.New("ITEM_NOT_ENOUGH")
	ErrUserNotFound                   = errors.New("USER_NOT_FOUND")
	ErrSecurityPepperNotConfigured    = errors.New("SECURITY_PEPPER_NOT_CONFIGURED")
	ErrAlreadySigned                  = errors.New("ALREADY_SIGNED")
	ErrSignDateInFuture               = errors.New("SIGN_DATE_IN_FUTURE")
	ErrConcurrentSignInRetry          = errors.New("CONCURRENT_SIGN_IN_RETRY")
	ErrInvalidInviteCode              = errors.New("INVALID_INVITE_CODE")
	ErrInvalidRenewCode               = errors.New("INVALID_RENEW_CODE")
	ErrTelegramUserMissing            = errors.New("TELEGRAM_USER_MISSING")
	ErrAbsUserIDEmpty                 = errors.New("ABS_USER_ID_EMPTY")
	ErrUsernameTaken                  = errors.New("USERNAME_TAKEN")
	ErrUsernameUnchanged              = errors.New("USERNAME_UNCHANGED")
	ErrAbsRefreshFailed               = errors.New("ABS_REFRESH_FAILED")
	ErrAbsRefreshFailedUsingCache     = errors.New("ABS_REFRESH_FAILED_USING_CACHE")
	ErrDailyListeningStatsReadFailed  = errors.New("DAILY_LISTENING_STATS_READ_FAILED")
	ErrTargetIsSuperAdmin             = errors.New("target_is_super_admin")
	ErrAdjustNoEffect                 = errors.New("adjust_no_effect")
	ErrDailyAdjustLimitExceeded       = errors.New("daily_adjust_limit_exceeded")
	ErrAlreadyInSect                  = errors.New("ALREADY_IN_SECT")
	ErrSectNameExists                 = errors.New("SECT_NAME_EXISTS")
	ErrSectNotFound                   = errors.New("SECT_NOT_FOUND")
	ErrSectFull                       = errors.New("SECT_FULL")
	ErrNotInSect                      = errors.New("NOT_IN_SECT")
	ErrTargetNotInSect                = errors.New("TARGET_NOT_IN_SECT")
	ErrSectNoPermission               = errors.New("NO_PERMISSION")
	ErrSectSameName                   = errors.New("SAME_NAME")
	ErrSectFundsNotEnough             = errors.New("FUNDS_NOT_ENOUGH")
	ErrSectOnlyOwner                  = errors.New("ONLY_OWNER")
	ErrSectMaxLevel                   = errors.New("MAX_LEVEL")
	ErrSectPrestigeNotEnough          = errors.New("PRESTIGE_NOT_ENOUGH")
	ErrSectResourceNotEnough          = errors.New("RESOURCE_NOT_ENOUGH")
	ErrSectCannotAppointOwner         = errors.New("CANNOT_APPOINT_OWNER")
	ErrSectCaveLocked                 = errors.New("SECT_CAVE_LOCKED")
	ErrSectCaveAlreadyUnlocked        = errors.New("SECT_CAVE_ALREADY_UNLOCKED")
	ErrSectPersonalPrestigeNotEnough  = errors.New("PERSONAL_PRESTIGE_NOT_ENOUGH")
	ErrSectContributionNotEnough      = errors.New("CONTRIBUTION_NOT_ENOUGH")
	ErrSectRetreatActive              = errors.New("SECT_RETREAT_ACTIVE")
	ErrSectRetreatNoEligibleMembers   = errors.New("SECT_RETREAT_NO_ELIGIBLE_MEMBERS")
	ErrSectShopRenewMonthlyLimit      = errors.New("SECT_SHOP_RENEW_MONTHLY_LIMIT")
	ErrSectShopRenewSectLimit         = errors.New("SECT_SHOP_RENEW_SECT_LIMIT")
	ErrSectShopRenewJoinedTooRecent   = errors.New("SECT_SHOP_RENEW_JOINED_TOO_RECENT")
	ErrSectShopRenewHistoryTooLow     = errors.New("SECT_SHOP_RENEW_HISTORY_TOO_LOW")
	ErrSectShopRenewExpireLimit       = errors.New("SECT_SHOP_RENEW_EXPIRE_LIMIT")
	ErrSectShopRenewPermanent         = errors.New("SECT_SHOP_RENEW_PERMANENT")
	ErrSectDailyTaskNotAllCompleted   = errors.New("SECT_DAILY_TASK_NOT_ALL_COMPLETED")
	ErrSectDailyTaskAlreadyClaimed    = errors.New("ALREADY_CLAIMED")
	ErrSectWeeklyTaskNotAchieved      = errors.New("SECT_WEEKLY_TASK_NOT_ACHIEVED")
	ErrSectWeeklyTaskAlreadySettled   = errors.New("SECT_WEEKLY_TASK_ALREADY_SETTLED")
	ErrSectLotteryContributionTooLow  = errors.New("SECT_LOTTERY_CONTRIBUTION_TOO_LOW")
	ErrSectLotteryUserIneligible      = errors.New("SECT_LOTTERY_USER_INELIGIBLE")
	ErrCultivationNotFound            = errors.New("CULTIVATION_NOT_FOUND")
	ErrMaxRealmReached                = errors.New("MAX_REALM_REACHED")
	ErrConsolidating                  = errors.New("CONSOLIDATING")
	ErrBreakthroughNotReady           = errors.New("NOT_READY")
	ErrInsufficientCultivation        = errors.New("INSUFFICIENT_CULTIVATION")
	ErrNoBreakthroughPill             = errors.New("NO_PILL")
	ErrInvalidBreakthroughMode        = errors.New("INVALID_BREAKTHROUGH_MODE")
	ErrCultivationStateChanged        = errors.New("CULTIVATION_STATE_CHANGED")
	ErrRandomFailed                   = errors.New("RANDOM_FAILED")
	ErrInvalidMarketplaceListing      = errors.New("INVALID_MARKETPLACE_LISTING")
	ErrMarketplaceDuplicateSecret     = errors.New("MARKETPLACE_DUPLICATE_SECRET")
	ErrMarketplaceInventoryNotEnough  = errors.New("MARKETPLACE_INVENTORY_NOT_ENOUGH")
	ErrMarketplaceListingNotFound     = errors.New("MARKETPLACE_LISTING_NOT_FOUND")
	ErrMarketplaceSelfBuy             = errors.New("MARKETPLACE_SELF_BUY")
	ErrMarketplaceOutOfStock          = errors.New("MARKETPLACE_OUT_OF_STOCK")
	ErrMarketplaceQuantityTooLarge    = errors.New("MARKETPLACE_QUANTITY_TOO_LARGE")
	ErrMarketplaceInvalidPrice        = errors.New("MARKETPLACE_INVALID_PRICE")
	ErrMarketplaceInvalidType         = errors.New("MARKETPLACE_INVALID_TYPE")
	ErrMarketplaceRealmTooLow         = errors.New("MARKETPLACE_REALM_TOO_LOW")
	ErrMarketplaceSellerMismatch      = errors.New("MARKETPLACE_SELLER_MISMATCH")
	ErrMarketplaceMixedSecretSource   = errors.New("MARKETPLACE_MIXED_SECRET_SOURCE")
	ErrMarketplaceVerifiedInvalid     = errors.New("MARKETPLACE_VERIFIED_SECRET_INVALID")
	ErrMarketplaceUnverifiedSecret    = errors.New("MARKETPLACE_UNVERIFIED_SECRET")
	ErrMarketplaceSecretOwnerMismatch = errors.New("MARKETPLACE_SECRET_OWNER_MISMATCH")
	ErrMarketplacePriceBelowFloor     = errors.New("MARKETPLACE_PRICE_BELOW_FLOOR")
	ErrMarketplacePriceAboveCeiling   = errors.New("MARKETPLACE_PRICE_ABOVE_CEILING")
	ErrCreateInviteCodeFailed         = errors.New("CREATE_INVITE_CODE_FAILED")
	ErrCreateRenewCodeFailed          = errors.New("CREATE_RENEW_CODE_FAILED")
	ErrCodeOwnerMismatch              = errors.New("CODE_OWNER_MISMATCH")
	ErrRenewCodeOwnerMismatch         = ErrCodeOwnerMismatch
	ErrCreateRedPacketFailed          = errors.New("CREATE_REDPACKET_FAILED")
	ErrGardenPlotMax                  = errors.New("GARDEN_PLOT_MAX")
	ErrGardenDailyLimit               = errors.New("GARDEN_DAILY_LIMIT")
	ErrGardenSeedNotAvailable         = errors.New("GARDEN_SEED_NOT_AVAILABLE")
	ErrGardenSeedUnknown              = errors.New("GARDEN_SEED_UNKNOWN")
	ErrGardenPlotNotFound             = errors.New("GARDEN_PLOT_NOT_FOUND")
	ErrGardenPlotBusy                 = errors.New("GARDEN_PLOT_BUSY")
	ErrGardenNoEmptyPlot              = errors.New("GARDEN_NO_EMPTY_PLOT")
	ErrGardenSeedNotEnough            = errors.New("GARDEN_SEED_NOT_ENOUGH")
	ErrGardenNoActivePlant            = errors.New("GARDEN_NO_ACTIVE_PLANT")
	ErrGardenNotMature                = errors.New("GARDEN_NOT_MATURE")
	ErrGardenAlreadyHarvested         = errors.New("GARDEN_ALREADY_HARVESTED")
	ErrGardenNoMaturePlant            = errors.New("GARDEN_NO_MATURE_PLANT")
	ErrGardenHerbNotSellable          = errors.New("GARDEN_HERB_NOT_SELLABLE")
	ErrGardenHerbNotEnough            = errors.New("GARDEN_HERB_NOT_ENOUGH")
	ErrGardenHerbQuantityInvalid      = errors.New("GARDEN_HERB_QUANTITY_INVALID")
	ErrGardenRecipeUnknown            = errors.New("GARDEN_RECIPE_UNKNOWN")
	ErrGardenRecipeUnlocked           = errors.New("GARDEN_RECIPE_UNLOCKED")
	ErrGardenRecipeLocked             = errors.New("GARDEN_RECIPE_LOCKED")
	ErrGardenMaterialNotEnough        = errors.New("GARDEN_MATERIAL_NOT_ENOUGH")
)

func FallbackBusinessErrorCode(err error) string {
	if err == nil {
		return ""
	}

	code := strings.TrimSpace(err.Error())
	if isKnownBusinessErrorCode(code) {
		return code
	}
	return unknownBusinessErrorCode
}

func isKnownBusinessErrorCode(code string) bool {
	switch code {
	case ErrLotteryNotActive.Error(),
		ErrLotteryWaitingDraw.Error(),
		ErrLotteryFull.Error(),
		ErrLotteryAlreadyJoined.Error(),
		ErrLotteryClaimExpired.Error(),
		ErrMarketplaceCloseNotFound.Error(),
		ErrSectSecretRealmNotActive.Error(),
		ErrSectSecretRealmAlreadyActive.Error(),
		ErrSectSecretRealmWeeklyLimit.Error(),
		ErrSectTechnologyLevelChanged.Error(),
		ErrPointsNotEnough.Error(),
		ErrInsufficientPoints.Error(),
		ErrAlreadyGrabbed.Error(),
		ErrConcurrentRedPacketGrabRetry.Error(),
		errAlreadyBet.Error(),
		ErrUsageLimitReached.Error(),
		ErrItemNotEnough.Error(),
		ErrUserNotFound.Error(),
		ErrSecurityPepperNotConfigured.Error(),
		ErrAlreadySigned.Error(),
		ErrSignDateInFuture.Error(),
		ErrConcurrentSignInRetry.Error(),
		ErrInvalidInviteCode.Error(),
		ErrInvalidRenewCode.Error(),
		ErrTelegramUserMissing.Error(),
		ErrAbsUserIDEmpty.Error(),
		ErrAbsRefreshFailed.Error(),
		ErrAbsRefreshFailedUsingCache.Error(),
		ErrTargetIsSuperAdmin.Error(),
		ErrAdjustNoEffect.Error(),
		ErrDailyAdjustLimitExceeded.Error(),
		ErrAlreadyInSect.Error(),
		ErrSectNameExists.Error(),
		ErrSectNotFound.Error(),
		ErrSectFull.Error(),
		ErrNotInSect.Error(),
		ErrTargetNotInSect.Error(),
		ErrSectNoPermission.Error(),
		ErrSectSameName.Error(),
		ErrSectFundsNotEnough.Error(),
		ErrSectOnlyOwner.Error(),
		ErrSectMaxLevel.Error(),
		ErrSectPrestigeNotEnough.Error(),
		ErrSectResourceNotEnough.Error(),
		ErrSectCannotAppointOwner.Error(),
		ErrSectCaveLocked.Error(),
		ErrSectCaveAlreadyUnlocked.Error(),
		ErrSectPersonalPrestigeNotEnough.Error(),
		ErrSectContributionNotEnough.Error(),
		ErrSectRetreatActive.Error(),
		ErrSectRetreatNoEligibleMembers.Error(),
		ErrSectShopRenewMonthlyLimit.Error(),
		ErrSectShopRenewSectLimit.Error(),
		ErrSectShopRenewJoinedTooRecent.Error(),
		ErrSectShopRenewHistoryTooLow.Error(),
		ErrSectShopRenewExpireLimit.Error(),
		ErrSectShopRenewPermanent.Error(),
		ErrSectDailyTaskNotAllCompleted.Error(),
		ErrSectDailyTaskAlreadyClaimed.Error(),
		ErrSectWeeklyTaskNotAchieved.Error(),
		ErrSectWeeklyTaskAlreadySettled.Error(),
		ErrSectLotteryContributionTooLow.Error(),
		ErrSectLotteryUserIneligible.Error(),
		ErrCultivationNotFound.Error(),
		ErrMaxRealmReached.Error(),
		ErrConsolidating.Error(),
		ErrBreakthroughNotReady.Error(),
		ErrInsufficientCultivation.Error(),
		ErrNoBreakthroughPill.Error(),
		ErrInvalidBreakthroughMode.Error(),
		ErrCultivationStateChanged.Error(),
		ErrRandomFailed.Error(),
		ErrInvalidMarketplaceListing.Error(),
		ErrMarketplaceDuplicateSecret.Error(),
		ErrMarketplaceInventoryNotEnough.Error(),
		ErrMarketplaceListingNotFound.Error(),
		ErrMarketplaceSelfBuy.Error(),
		ErrMarketplaceOutOfStock.Error(),
		ErrMarketplaceQuantityTooLarge.Error(),
		ErrMarketplaceInvalidPrice.Error(),
		ErrMarketplaceInvalidType.Error(),
		ErrMarketplaceRealmTooLow.Error(),
		ErrMarketplaceSellerMismatch.Error(),
		ErrMarketplaceMixedSecretSource.Error(),
		ErrMarketplaceVerifiedInvalid.Error(),
		ErrMarketplaceUnverifiedSecret.Error(),
		ErrMarketplaceSecretOwnerMismatch.Error(),
		ErrMarketplacePriceBelowFloor.Error(),
		ErrMarketplacePriceAboveCeiling.Error(),
		ErrSectHornInvalidScope.Error(),
		ErrSectHornNoRecipients.Error(),
		ErrSectHornContentEmpty.Error(),
		ErrSectHornContentShort.Error(),
		ErrSectHornContentLong.Error(),
		ErrSectHornControlChar.Error(),
		ErrSectHornLinkBlocked.Error(),
		"SECT_HORN_COOLDOWN",
		ErrCreateInviteCodeFailed.Error(),
		ErrCreateRenewCodeFailed.Error(),
		ErrRenewCodeOwnerMismatch.Error(),
		ErrCreateRedPacketFailed.Error(),
		ErrGardenPlotMax.Error(),
		ErrGardenDailyLimit.Error(),
		ErrGardenSeedNotAvailable.Error(),
		ErrGardenSeedUnknown.Error(),
		ErrGardenPlotNotFound.Error(),
		ErrGardenPlotBusy.Error(),
		ErrGardenNoEmptyPlot.Error(),
		ErrGardenSeedNotEnough.Error(),
		ErrGardenNoActivePlant.Error(),
		ErrGardenNotMature.Error(),
		ErrGardenAlreadyHarvested.Error(),
		ErrGardenNoMaturePlant.Error(),
		ErrGardenHerbNotSellable.Error(),
		ErrGardenHerbNotEnough.Error(),
		ErrGardenHerbQuantityInvalid.Error(),
		ErrGardenRecipeUnknown.Error(),
		ErrGardenRecipeUnlocked.Error(),
		ErrGardenRecipeLocked.Error(),
		ErrGardenMaterialNotEnough.Error():
		return true
	default:
		return false
	}
}

func AssetCreationErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrUserNotFound):
		return "USER_NOT_FOUND"
	case errors.Is(err, ErrTrialCannotUseRenewCode):
		return "TRIAL_CANNOT_USE_RENEW_CODE"
	case errors.Is(err, ErrRenewCodeOwnerMismatch):
		return "RENEW_CODE_OWNER_MISMATCH"
	case errors.Is(err, ErrSecurityPepperNotConfigured):
		return "SECURITY_PEPPER_NOT_CONFIGURED"
	case errors.Is(err, ErrCreateInviteCodeFailed):
		return "CREATE_INVITE_CODE_FAILED"
	case errors.Is(err, ErrCreateRenewCodeFailed):
		return "CREATE_RENEW_CODE_FAILED"
	case errors.Is(err, ErrCreateRedPacketFailed):
		return "CREATE_REDPACKET_FAILED"
	case errors.Is(err, ErrInsufficientPoints):
		return "INSUFFICIENT_POINTS"
	case err != nil:
		return FallbackBusinessErrorCode(err)
	default:
		return ""
	}
}

func SignInErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrAlreadySigned):
		return "ALREADY_SIGNED"
	case errors.Is(err, ErrSignDateInFuture):
		return "SIGN_DATE_IN_FUTURE"
	case errors.Is(err, ErrConcurrentSignInRetry):
		return "CONCURRENT_SIGN_IN_RETRY"
	case err != nil:
		return FallbackBusinessErrorCode(err)
	default:
		return ""
	}
}

func RenewRedeemErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrInvalidRenewCode):
		return "INVALID_RENEW_CODE"
	case errors.Is(err, ErrUserNotFound):
		return "USER_NOT_FOUND"
	case errors.Is(err, ErrTrialCannotUseRenewCode):
		return "TRIAL_CANNOT_USE_RENEW_CODE"
	case errors.Is(err, ErrSecurityPepperNotConfigured):
		return "SECURITY_PEPPER_NOT_CONFIGURED"
	case err != nil:
		return FallbackBusinessErrorCode(err)
	default:
		return ""
	}
}

var (
	ErrReferralInvalidCode        = errors.New("REFERRAL_INVALID_CODE")
	ErrReferralSelfInvite         = errors.New("REFERRAL_SELF_INVITE")
	ErrReferralInviterNotEligible = errors.New("REFERRAL_INVITER_NOT_ELIGIBLE")
	ErrReferralDailyLimit         = errors.New("REFERRAL_DAILY_LIMIT")
	ErrReferralAlreadyTried       = errors.New("REFERRAL_ALREADY_TRIED")
	ErrReferralExistingAccount    = errors.New("REFERRAL_EXISTING_ACCOUNT")
	ErrReferralNoActivation       = errors.New("REFERRAL_NO_ACTIVATION")
	ErrReferralTrialExpired       = errors.New("REFERRAL_TRIAL_EXPIRED")
	ErrReferralTaskNotComplete    = errors.New("REFERRAL_TASK_NOT_COMPLETE")
	ErrReferralAlreadyEffective   = errors.New("REFERRAL_ALREADY_EFFECTIVE")
	ErrReferralCultivationLow     = errors.New("REFERRAL_CULTIVATION_LOW")
	ErrTrialCannotUseRenewCode    = errors.New("TRIAL_CANNOT_USE_RENEW_CODE")
	ErrTrialFormalInviteOnly      = errors.New("TRIAL_FORMAL_INVITE_ONLY")
)

var (
	ErrSectHornInvalidScope = errors.New("SECT_HORN_INVALID_SCOPE")
	ErrSectHornNoRecipients = errors.New("SECT_HORN_NO_RECIPIENTS")
	ErrSectHornContentEmpty = errors.New("SECT_HORN_CONTENT_EMPTY")
	ErrSectHornContentShort = errors.New("SECT_HORN_CONTENT_SHORT")
	ErrSectHornContentLong  = errors.New("SECT_HORN_CONTENT_LONG")
	ErrSectHornControlChar  = errors.New("SECT_HORN_CONTROL_CHAR")
	ErrSectHornLinkBlocked  = errors.New("SECT_HORN_LINK_BLOCKED")
)
