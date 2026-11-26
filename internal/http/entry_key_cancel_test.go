package http

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"finnri/internal/database"
	"finnri/internal/models"
)

type entryKeyCancel struct {
	Saved     bool          `json:"saved"`
	Cancelled bool          `json:"cancelled"`
	Entry     *models.Entry `json:"entry"`
}

func postWithKey(router *gin.Engine, token, key string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]any{
		"title": "Day's spends", "type": "expense", "amount": 300, "currency": "INR",
		"source": "manual", "mode": "UPI", "category": "Misc", "date": "2026-11-18",
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/entries", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", key)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func createdEntry(t *testing.T, router *gin.Engine, token, key string) models.Entry {
	t.Helper()
	response := postWithKey(router, token, key)
	if response.Code != http.StatusCreated {
		t.Fatalf("create returned %d: %s", response.Code, response.Body.String())
	}
	var entry models.Entry
	if err := json.Unmarshal(response.Body.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	return entry
}

// A capture tray that lost the answer to a save cancels the key before saving
// that money another way. The answer must be final: either the entry is in the
// ledger, or the key can never put it there.
func TestCancelEntryKey(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)
	guest := func(device string) AuthResponse {
		return performJSONRequest[AuthResponse](
			t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{"device_id": device}, http.StatusOK,
		)
	}
	owner := guest("key-cancel-owner")
	other := guest("key-cancel-other")
	cancel := func(token, key string, status int) entryKeyCancel {
		return performJSONRequest[entryKeyCancel](
			t, router, http.MethodPost, "/v1/entry-keys/"+key+"/cancel", token, nil, status,
		)
	}
	entryCount := func(userID uint) int64 {
		var count int64
		database.DB.Model(&models.Entry{}).Where("user_id = ?", userID).Count(&count)
		return count
	}

	t.Run("a cancelled key never saves, however late the create arrives", func(t *testing.T) {
		key := "day-abc-total-0011223344556677"
		for range 2 { // asking again gives the same answer
			if got := cancel(owner.Token, key, http.StatusOK); got.Saved || !got.Cancelled || got.Entry != nil {
				t.Fatalf("expected cancelled, got %#v", got)
			}
		}
		response := postWithKey(router, owner.Token, key)
		if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "entry_key_cancelled") {
			t.Fatalf("late create returned %d: %s", response.Code, response.Body.String())
		}
		if got := entryCount(owner.User.ID); got != 0 {
			t.Fatalf("a cancelled key wrote %d entries", got)
		}
	})

	t.Run("a key already saved is reported, not cancelled", func(t *testing.T) {
		key := "day-abc-saved"
		created := createdEntry(t, router, owner.Token, key)
		got := cancel(owner.Token, key, http.StatusOK)
		if !got.Saved || got.Cancelled || got.Entry == nil || got.Entry.ID != created.ID {
			t.Fatalf("expected entry %d, got %#v", created.ID, got)
		}
		if replay := postWithKey(router, owner.Token, key); replay.Code != http.StatusOK {
			t.Fatalf("replay returned %d: %s", replay.Code, replay.Body.String())
		}
	})

	t.Run("a key is the caller's own", func(t *testing.T) {
		key := "day-abc-shared"
		cancel(other.Token, key, http.StatusOK)
		createdEntry(t, router, owner.Token, key)
		if got := cancel(other.Token, key, http.StatusOK); got.Saved || got.Entry != nil {
			t.Fatalf("another user's entry is visible: %#v", got)
		}
	})

	t.Run("deleting the entry leaves the key usable", func(t *testing.T) {
		key := "day-abc-deleted"
		created := createdEntry(t, router, owner.Token, key)
		performJSONRequest[map[string]any](
			t, router, http.MethodDelete, "/v1/entries/"+strconv.FormatUint(uint64(created.ID), 10), owner.Token, nil, http.StatusOK,
		)
		createdEntry(t, router, owner.Token, key)
	})

	cancel("", "day-abc", http.StatusUnauthorized)
	cancel(owner.Token, strings.Repeat("k", 129), http.StatusUnprocessableEntity)

	t.Run("account deletion removes the keys", func(t *testing.T) {
		var user models.User
		if err := database.DB.First(&user, owner.User.ID).Error; err != nil {
			t.Fatal(err)
		}
		if _, err := deleteUserData(database.DB, user); err != nil {
			t.Fatal(err)
		}
		var left int64
		database.DB.Model(&models.EntryKeyClaim{}).Where("user_id = ?", user.ID).Count(&left)
		if left != 0 {
			t.Fatalf("%d key claims left after account deletion", left)
		}
	})
}
