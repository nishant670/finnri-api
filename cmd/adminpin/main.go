// Command adminpin mints the SQL that grants an existing account access to the
// admin console.
//
// It exists because the console signs in with email + PIN, while every real
// account so far was created through Google sign-in — which never sets a PIN —
// and the OTP flow that would set one is deliberately switched off in
// production. That leaves no in-product path to the first admin, so the first
// one is made here and every later one can be made from inside the console.
//
// It only prints SQL. It never connects to a database, so running it is safe
// and the statement can be read before anything is executed.
//
//	go run ./cmd/adminpin -email you@gmail.com -pin 4917
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

func validPIN(pin string) error {
	if len(pin) != 4 {
		return fmt.Errorf("a PIN is exactly 4 digits")
	}
	for _, char := range pin {
		if char < '0' || char > '9' {
			return fmt.Errorf("a PIN is digits only")
		}
	}
	if strings.Count(pin, string(pin[0])) == 4 {
		return fmt.Errorf("a PIN cannot be the same digit four times")
	}
	return nil
}

func main() {
	email := flag.String("email", "", "the email of the existing, non-guest account to promote")
	pin := flag.String("pin", "", "the 4-digit PIN that account will sign into /admin/login with")
	flag.Parse()

	if strings.TrimSpace(*email) == "" || *pin == "" {
		fmt.Fprintln(os.Stderr, "usage: go run ./cmd/adminpin -email you@gmail.com -pin 4917")
		os.Exit(2)
	}
	if err := validPIN(*pin); err != nil {
		fmt.Fprintf(os.Stderr, "refusing to hash that PIN: %v\n", err)
		os.Exit(2)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(*pin), bcrypt.DefaultCost)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bcrypt failed: %v\n", err)
		os.Exit(1)
	}

	// Lower-cased because adminLogin looks the account up with LOWER(email) = ?.
	lowered := strings.ToLower(strings.TrimSpace(*email))
	fmt.Printf(`-- Run against the production database. Both statements are idempotent.
-- 1. Give the account a PIN. Prints 1 row; 0 rows means no such non-guest account.
UPDATE users
   SET pin_hash = '%s',
       failed_login_attempts = 0,
       login_locked_until = NULL,
       updated_at = NOW()
 WHERE LOWER(email) = '%s'
   AND is_guest = false
RETURNING id, email;

-- 2. Read the id above, then set ADMIN_BOOTSTRAP_USER_IDS to it on Railway and
--    redeploy. The backend creates the owner row itself on every boot, and
--    re-enables it if it was ever disabled.
--    Prefer that to inserting into admin_users by hand.
`, string(hash), strings.ReplaceAll(lowered, "'", "''"))
}
