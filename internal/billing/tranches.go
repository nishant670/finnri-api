package billing

import (
	"fmt"
	"time"

	"finnri/internal/models"
)

const (
	SubscriptionTrancheDays       = 30
	QuarterlyTrancheCredits       = 3667
	YearlyTrancheCredits          = 4000
	LifetimeMonthlyTrancheCredits = 5000
)

type subscriptionTranchePolicy struct {
	credits       int
	count         int
	rolloverCount int
}

// SyncSubscriptionTranches issues only the monthly slices due as of now. The
// grant's valid_from is the idempotency boundary, so running this every minute
// is safe. Quarterly and yearly users may carry one prior slice; lifetime
// grants expire when the next slice begins and therefore never roll over.
func (s *CreditService) SyncSubscriptionTranches(subscription models.UserSubscription, now time.Time) (int, error) {
	if subscription.ID == 0 || subscription.UserID == 0 {
		return 0, fmt.Errorf("subscription and user ids are required")
	}
	if subscription.Status != "active" || now.Before(subscription.CurrentPeriodStart) {
		return 0, nil
	}
	if subscription.Plan.ID == 0 {
		if err := s.db.First(&subscription.Plan, subscription.PlanID).Error; err != nil {
			return 0, err
		}
	}

	policy, supported := tranchePolicyForInterval(subscription.Plan.BillingInterval)
	if !supported {
		return 0, nil
	}
	if subscription.Plan.BillingInterval != "lifetime_quote" && !now.Before(subscription.CurrentPeriodEnd) {
		return 0, nil
	}
	elapsed := now.Sub(subscription.CurrentPeriodStart)
	currentIndex := int(elapsed / (SubscriptionTrancheDays * 24 * time.Hour))
	if currentIndex < 0 {
		return 0, nil
	}

	firstIndex := 0
	lastIndex := currentIndex
	if policy.count > 0 {
		if lastIndex >= policy.count {
			lastIndex = policy.count - 1
		}
	} else {
		// A lifetime subscription may be years old. Earlier no-rollover grants
		// are already unusable, so creating a historical ledger for all of them
		// adds cost without adding credits the user can spend.
		firstIndex = lastIndex
	}
	if lastIndex < firstIndex {
		return 0, nil
	}

	created := 0
	for index := firstIndex; index <= lastIndex; index++ {
		validFrom := subscription.CurrentPeriodStart.Add(time.Duration(index*SubscriptionTrancheDays) * 24 * time.Hour)
		expiresAt := validFrom.Add(time.Duration((policy.rolloverCount+1)*SubscriptionTrancheDays) * 24 * time.Hour)
		_, didCreate, err := s.GrantSubscriptionPeriod(subscription, policy.credits, validFrom, expiresAt)
		if err != nil {
			return created, err
		}
		if didCreate {
			created++
		}
	}
	return created, nil
}

func tranchePolicyForInterval(interval string) (subscriptionTranchePolicy, bool) {
	switch interval {
	case "quarterly":
		return subscriptionTranchePolicy{credits: QuarterlyTrancheCredits, count: 3, rolloverCount: 1}, true
	case "yearly":
		return subscriptionTranchePolicy{credits: YearlyTrancheCredits, count: 12, rolloverCount: 1}, true
	case "lifetime_quote":
		return subscriptionTranchePolicy{credits: LifetimeMonthlyTrancheCredits, rolloverCount: 0}, true
	default:
		return subscriptionTranchePolicy{}, false
	}
}
