package http

import (
	"testing"

	"finnri/internal/models"
)

// rupees builds a Money from whole rupees, so the tables below read like the
// figures a user would see.
func rupees(amount int64) models.Money { return models.Money(amount * 100) }

func TestSummariseCardLimit(t *testing.T) {
	cases := []struct {
		name            string
		input           cardLimitInput
		wantOutstanding models.Money
		wantSource      string
		wantAvailable   *models.Money
		wantUtilisation *float64
	}{
		{
			name: "no statement yet falls back to the ledger",
			input: cardLimitInput{
				CreditLimit:       rupees(200000),
				LedgerOutstanding: rupees(15520),
			},
			wantOutstanding: rupees(15520),
			wantSource:      outstandingFromLedger,
			wantAvailable:   ptrMoney(rupees(184480)),
			wantUtilisation: ptrFloat(7.76),
		},
		{
			name: "statement wins over the ledger once it has an amount",
			input: cardLimitInput{
				CreditLimit:         rupees(200000),
				HasStatement:        true,
				StatementTotalDue:   rupees(12400),
				SpendAfterStatement: rupees(3120),
				// Deliberately disagrees: the ledger is missing spends the
				// bank knows about, which is exactly why the statement wins.
				LedgerOutstanding: rupees(11320),
			},
			wantOutstanding: rupees(15520),
			wantSource:      outstandingFromStatement,
			wantAvailable:   ptrMoney(rupees(184480)),
			wantUtilisation: ptrFloat(7.76),
		},
		{
			name: "a partial payment reduces what is owed",
			input: cardLimitInput{
				CreditLimit:         rupees(200000),
				HasStatement:        true,
				StatementTotalDue:   rupees(12400),
				StatementPaid:       rupees(5000),
				SpendAfterStatement: rupees(3120),
			},
			wantOutstanding: rupees(10520),
			wantSource:      outstandingFromStatement,
			wantAvailable:   ptrMoney(rupees(189480)),
			wantUtilisation: ptrFloat(5.26),
		},
		{
			name: "unbilled EMI principal blocks the limit",
			input: cardLimitInput{
				CreditLimit:         rupees(200000),
				HasStatement:        true,
				StatementTotalDue:   rupees(5000),
				EMIBlockedPrincipal: rupees(55000),
			},
			wantOutstanding: rupees(5000),
			wantSource:      outstandingFromStatement,
			wantAvailable:   ptrMoney(rupees(140000)),
			wantUtilisation: ptrFloat(30),
		},
		{
			name: "paying the bill releases that month's EMI principal",
			input: cardLimitInput{
				CreditLimit:         rupees(200000),
				HasStatement:        true,
				StatementTotalDue:   rupees(5000),
				StatementPaid:       rupees(5000),
				EMIBlockedPrincipal: rupees(55000),
			},
			wantOutstanding: 0,
			wantSource:      outstandingFromStatement,
			wantAvailable:   ptrMoney(rupees(145000)),
			wantUtilisation: ptrFloat(27.5),
		},
		{
			name: "over the limit is reported honestly",
			input: cardLimitInput{
				CreditLimit:       rupees(50000),
				HasStatement:      true,
				StatementTotalDue: rupees(55000),
			},
			wantOutstanding: rupees(55000),
			wantSource:      outstandingFromStatement,
			wantAvailable:   ptrMoney(rupees(-5000)),
			wantUtilisation: ptrFloat(110),
		},
		{
			name: "a card in credit frees the whole limit",
			input: cardLimitInput{
				CreditLimit:       rupees(200000),
				HasStatement:      true,
				StatementTotalDue: rupees(12400),
				StatementPaid:     rupees(15000),
			},
			wantOutstanding: rupees(-2600),
			wantSource:      outstandingFromStatement,
			wantAvailable:   ptrMoney(rupees(200000)),
			wantUtilisation: ptrFloat(0),
		},
		{
			name: "no limit entered reports outstanding but invents nothing",
			input: cardLimitInput{
				HasStatement:      true,
				StatementTotalDue: rupees(12400),
			},
			wantOutstanding: rupees(12400),
			wantSource:      outstandingFromStatement,
			wantAvailable:   nil,
			wantUtilisation: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := summariseCardLimit(tc.input)

			if got.Outstanding != tc.wantOutstanding {
				t.Errorf("outstanding = %s, want %s", got.Outstanding, tc.wantOutstanding)
			}
			if got.OutstandingSource != tc.wantSource {
				t.Errorf("outstanding source = %q, want %q", got.OutstandingSource, tc.wantSource)
			}
			switch {
			case tc.wantAvailable == nil && got.AvailableLimit != nil:
				t.Errorf("available limit = %s, want absent", *got.AvailableLimit)
			case tc.wantAvailable != nil && got.AvailableLimit == nil:
				t.Errorf("available limit absent, want %s", *tc.wantAvailable)
			case tc.wantAvailable != nil && *got.AvailableLimit != *tc.wantAvailable:
				t.Errorf("available limit = %s, want %s", *got.AvailableLimit, *tc.wantAvailable)
			}
			switch {
			case tc.wantUtilisation == nil && got.UtilisationPct != nil:
				t.Errorf("utilisation = %.2f, want absent", *got.UtilisationPct)
			case tc.wantUtilisation != nil && got.UtilisationPct == nil:
				t.Errorf("utilisation absent, want %.2f", *tc.wantUtilisation)
			case tc.wantUtilisation != nil:
				if diff := *got.UtilisationPct - *tc.wantUtilisation; diff > 0.01 || diff < -0.01 {
					t.Errorf("utilisation = %.4f, want %.2f", *got.UtilisationPct, *tc.wantUtilisation)
				}
			}
		})
	}
}

// The limit must add up: what is available plus what is committed is the
// whole limit, unless the card is in credit (where committed floors at zero).
func TestAvailableLimitAndCommittedSumToLimit(t *testing.T) {
	input := cardLimitInput{
		CreditLimit:         rupees(200000),
		HasStatement:        true,
		StatementTotalDue:   rupees(12400),
		StatementPaid:       rupees(2400),
		SpendAfterStatement: rupees(3120),
		EMIBlockedPrincipal: rupees(55000),
	}
	got := summariseCardLimit(input)
	if got.AvailableLimit == nil {
		t.Fatal("available limit absent")
	}
	committed := got.Outstanding + got.EMIBlockedPrincipal
	if total := *got.AvailableLimit + committed; total != input.CreditLimit {
		t.Fatalf("available %s + committed %s = %s, want %s",
			*got.AvailableLimit, committed, total, input.CreditLimit)
	}
}

func ptrMoney(value models.Money) *models.Money { return &value }
func ptrFloat(value float64) *float64           { return &value }
