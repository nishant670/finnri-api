package http

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"finnri/internal/config"
	"finnri/internal/database"
	"finnri/internal/models"
)

type attachmentWorld struct {
	router                  *gin.Engine
	owner, member           models.User
	ownerToken, memberToken string
}

func newAttachmentWorld(t *testing.T) attachmentWorld {
	t.Helper()
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	var server *Server
	world := attachmentWorld{router: smokeRouter(t, func(s *Server, _ *config.Config) { server = s })}
	world.router.POST("/test/upload", server.handleUpload)
	world.router.DELETE("/v1/user", AuthMiddleware(), server.deleteUser)
	// Upload files are read, written and removed relative to the working directory.
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		t.Fatal(err)
	}
	world.owner, world.ownerToken = createPaidBillingTestUserSession(t)
	world.member, world.memberToken = createPaidBillingTestUserSession(t)
	return world
}

// upload sends a real file through the upload handler and returns the URL an app
// would then save on a record.
func (w attachmentWorld) upload(t *testing.T) string {
	t.Helper()
	request := uploadRequest(t, "receipt.png", "image/png", pngBytes())
	request.URL.Path = "/test/upload"
	response := httptest.NewRecorder()
	w.router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", response.Code, response.Body.String())
	}
	_, after, _ := strings.Cut(response.Body.String(), `"url":"`)
	url, _, _ := strings.Cut(after, `"`)
	return url
}

func (w attachmentWorld) request(t *testing.T, method, target, token string, body any) (int, string) {
	t.Helper()
	var encoded []byte
	if body != nil {
		var err error
		if encoded, err = json.Marshal(body); err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(method, target, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	w.router.ServeHTTP(response, request)
	return response.Code, response.Body.String()
}

func fileExists(url string) bool {
	_, err := os.Stat(filepath.Join(uploadDir, filepath.Base(url)))
	return err == nil
}

func entryPayload(attachment string) map[string]any {
	return map[string]any{
		"title": "Dinner", "type": "expense", "amount": "500", "mode": "Cash",
		"category": "Food & Drinks", "date": "2026-11-20", "attachment": attachment,
	}
}

func (w attachmentWorld) createEntry(t *testing.T, token, attachment string) models.Entry {
	t.Helper()
	return performJSONRequest[models.Entry](t, w.router, http.MethodPost, "/v1/entries", token, entryPayload(attachment), http.StatusCreated)
}

// sharedGroupReceipt is the starting point of SEC-14: the owner's group bill
// carries a receipt, and the member can legitimately see its name.
func (w attachmentWorld) sharedGroupReceipt(t *testing.T) (models.SplitGroup, string) {
	t.Helper()
	friend := performJSONRequest[models.SplitFriend](t, w.router, http.MethodPost, "/v1/split/friends", w.ownerToken,
		map[string]any{"name": "Riya"}, http.StatusCreated)
	group := performJSONRequest[models.SplitGroup](t, w.router, http.MethodPost, "/v1/split/groups", w.ownerToken,
		map[string]any{"name": "Goa", "kind": "trip", "friend_ids": []uint{friend.ID}}, http.StatusCreated)
	if err := database.DB.Create(&models.SplitGroupUserMember{
		GroupID: group.ID, UserID: w.member.ID, Role: "member", Status: "active",
	}).Error; err != nil {
		t.Fatal(err)
	}
	receipt := w.upload(t)
	performJSONRequest[models.SplitBill](t, w.router, http.MethodPost, "/v1/split/bills", w.ownerToken, map[string]any{
		"group_id": group.ID, "title": "Dinner", "total_amount": "900", "date": "2026-11-20", "attachment": receipt,
		"participants": []map[string]any{{"friend_id": friend.ID, "share_amount": "300", "direction": splitDirectionFriendOwesUser}},
	}, http.StatusCreated)
	if !userCanReadUpload(w.member.ID, filepath.Base(receipt)) {
		t.Fatal("setup: a group member should see the shared receipt")
	}
	return group, receipt
}

// Every route that stores a file name must refuse a name another user's record
// carries.
func TestAttachmentCannotBeAdoptedFromAnotherUser(t *testing.T) {
	w := newAttachmentWorld(t)
	_, receipt := w.sharedGroupReceipt(t)
	name := filepath.Base(receipt)
	// The same file spelled differently must not slip past.
	encoded := strings.Replace(receipt, name, fmt.Sprintf("%%%02x", name[0])+name[1:], 1)

	plain := w.createEntry(t, w.memberToken, "")
	memberFriend := performJSONRequest[models.SplitFriend](t, w.router, http.MethodPost, "/v1/split/friends", w.memberToken,
		map[string]any{"name": "Aman"}, http.StatusCreated)
	billPayload := func(attachment string) map[string]any {
		return map[string]any{
			"title": "Mine", "total_amount": "100", "date": "2026-11-20", "attachment": attachment,
			"participants": []map[string]any{{"friend_id": memberFriend.ID, "share_amount": "50", "direction": splitDirectionFriendOwesUser}},
		}
	}
	ownBill := performJSONRequest[models.SplitBill](t, w.router, http.MethodPost, "/v1/split/bills", w.memberToken, billPayload(""), http.StatusCreated)
	ownGroup := performJSONRequest[models.SplitGroup](t, w.router, http.MethodPost, "/v1/split/groups", w.memberToken,
		map[string]any{"name": "Mine", "kind": "trip"}, http.StatusCreated)
	feedback := func(attachment string) map[string]any {
		return map[string]any{"type": "bug", "impact": "high", "title": "t", "message": "m", "attachments": []string{attachment}}
	}

	for _, attempt := range []struct {
		name, method, target string
		body                 any
	}{
		{"new entry", http.MethodPost, "/v1/entries", entryPayload(receipt)},
		{"new entry, encoded name", http.MethodPost, "/v1/entries", entryPayload(encoded)},
		{"new entry, another host", http.MethodPost, "/v1/entries", entryPayload("https://elsewhere.example/uploads/" + name)},
		{"new entry, bare path", http.MethodPost, "/v1/entries", entryPayload("/uploads/" + name)},
		{"edit entry", http.MethodPut, fmt.Sprintf("/v1/entries/%d", plain.ID), map[string]any{"attachment": receipt}},
		{"new split bill", http.MethodPost, "/v1/split/bills", billPayload(receipt)},
		{"edit split bill", http.MethodPut, fmt.Sprintf("/v1/split/bills/%d", ownBill.ID), billPayload(receipt)},
		{"new group photo", http.MethodPost, "/v1/split/groups", map[string]any{"name": "Two", "kind": "trip", "photo_url": receipt}},
		{"edit group photo", http.MethodPut, fmt.Sprintf("/v1/split/groups/%d", ownGroup.ID), map[string]any{"name": "Mine", "kind": "trip", "photo_url": receipt}},
		{"feedback", http.MethodPost, "/v1/feedback", feedback(receipt)},
	} {
		status, body := w.request(t, attempt.method, attempt.target, w.memberToken, attempt.body)
		if status != http.StatusUnprocessableEntity {
			t.Errorf("%s: another user's receipt was accepted (%d %s)", attempt.name, status, body)
		}
	}
}

func TestAttachmentAccessEndsWhenMemberLeavesGroup(t *testing.T) {
	w := newAttachmentWorld(t)
	group, receipt := w.sharedGroupReceipt(t)

	w.request(t, http.MethodPost, "/v1/entries", w.memberToken, entryPayload(receipt))
	status, body := w.request(t, http.MethodPost, fmt.Sprintf("/v1/split/groups/%d/leave", group.ID), w.memberToken, nil)
	if status != http.StatusOK {
		t.Fatalf("leave: %d %s", status, body)
	}
	if userCanReadUpload(w.member.ID, filepath.Base(receipt)) {
		t.Fatal("the member kept access to the receipt after leaving the group")
	}
}

func TestDeletingAnAdoptedEntryKeepsTheOwnersFile(t *testing.T) {
	w := newAttachmentWorld(t)
	_, receipt := w.sharedGroupReceipt(t)

	status, _ := w.request(t, http.MethodPost, "/v1/entries", w.memberToken, entryPayload(receipt))
	var adopted []models.Entry
	if err := database.DB.Where("user_id = ?", w.member.ID).Find(&adopted).Error; err != nil {
		t.Fatal(err)
	}
	for _, entry := range adopted {
		w.request(t, http.MethodDelete, fmt.Sprintf("/v1/entries/%d", entry.ID), w.memberToken, nil)
	}
	t.Logf("adopting answered %d", status)
	if !fileExists(receipt) {
		t.Fatal("deleting the member's entry removed the owner's receipt")
	}
}

// Rows adopted before the ownership check existed are still in the database, so
// removing files has to be safe without it.
func (w attachmentWorld) forceSecondReference(t *testing.T, userID uint, attachment string) models.Entry {
	t.Helper()
	entry := models.Entry{
		UserID: userID, Title: "Old row", Type: "expense", Amount: 100, Currency: "INR",
		Mode: "Cash", Category: "Food & Drinks", Date: "2026-11-01", Attachment: attachment,
	}
	if err := database.DB.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}
	return entry
}

func TestFileIsKeptWhileAnyRecordUsesIt(t *testing.T) {
	remove := map[string]func(t *testing.T, w attachmentWorld, entry models.Entry){
		"delete entry": func(t *testing.T, w attachmentWorld, entry models.Entry) {
			w.request(t, http.MethodDelete, fmt.Sprintf("/v1/entries/%d", entry.ID), w.memberToken, nil)
		},
		"replace receipt": func(t *testing.T, w attachmentWorld, entry models.Entry) {
			status, body := w.request(t, http.MethodPut, fmt.Sprintf("/v1/entries/%d", entry.ID), w.memberToken,
				map[string]any{"attachment": w.upload(t)})
			if status != http.StatusOK {
				t.Fatalf("replace: %d %s", status, body)
			}
		},
		"delete account": func(t *testing.T, w attachmentWorld, _ models.Entry) {
			status, body := w.request(t, http.MethodDelete, "/v1/user", w.memberToken, nil)
			if status != http.StatusOK {
				t.Fatalf("delete account: %d %s", status, body)
			}
		},
	}
	// Each kind of record that can name a file, held by somebody else.
	holders := map[string]func(t *testing.T, w attachmentWorld, url string){
		"an entry": func(t *testing.T, w attachmentWorld, url string) {
			w.forceSecondReference(t, w.owner.ID, url)
		},
		"a split bill": func(t *testing.T, w attachmentWorld, url string) {
			mustCreate(t, &models.SplitBill{UserID: w.owner.ID, Title: "b", TotalAmount: 100, Currency: "INR", Date: "2026-11-01", Attachment: url})
		},
		"a group photo": func(t *testing.T, w attachmentWorld, url string) {
			mustCreate(t, &models.SplitGroup{UserID: w.owner.ID, Name: "g", Kind: "trip", PhotoURL: url})
		},
		"feedback": func(t *testing.T, w attachmentWorld, url string) {
			mustCreate(t, &models.Feedback{UserID: w.owner.ID, Type: "bug", Title: "t", Message: "m", Impact: "high", Status: "new", Attachments: models.StringArray{url}})
		},
	}
	for removal, run := range remove {
		for holder, hold := range holders {
			t.Run(removal+" while "+holder+" uses it", func(t *testing.T) {
				w := newAttachmentWorld(t)
				receipt := w.upload(t)
				hold(t, w, receipt)
				entry := w.forceSecondReference(t, w.member.ID, receipt)
				run(t, w, entry)
				if !fileExists(receipt) {
					t.Fatalf("%s removed a file %s still uses", removal, holder)
				}
			})
		}
		t.Run(removal+" of the only use", func(t *testing.T) {
			w := newAttachmentWorld(t)
			receipt := w.upload(t)
			entry := w.createEntry(t, w.memberToken, receipt)
			run(t, w, entry)
			if fileExists(receipt) {
				t.Fatalf("%s left an unused file behind", removal)
			}
		})
	}
}

func mustCreate(t *testing.T, value any) {
	t.Helper()
	if err := database.DB.Create(value).Error; err != nil {
		t.Fatal(err)
	}
}

// What the shipped apps do: upload then save, and send the saved URL back when
// the same record is edited.
func TestOwnAttachmentsKeepWorking(t *testing.T) {
	w := newAttachmentWorld(t)
	friend := performJSONRequest[models.SplitFriend](t, w.router, http.MethodPost, "/v1/split/friends", w.ownerToken,
		map[string]any{"name": "Riya"}, http.StatusCreated)

	receipt := w.upload(t)
	payload := entryPayload(receipt)
	payload["split"] = map[string]any{"participants": []map[string]any{{"friend_id": friend.ID, "share_amount": "200"}}}
	entry := performJSONRequest[models.Entry](t, w.router, http.MethodPost, "/v1/entries", w.ownerToken, payload, http.StatusCreated)
	path := fmt.Sprintf("/v1/entries/%d", entry.ID)
	bill := performJSONRequest[models.SplitBill](t, w.router, http.MethodGet, fmt.Sprintf("/v1/split/bills/by-entry/%d", entry.ID), w.ownerToken, nil, http.StatusOK)
	if bill.Attachment != receipt {
		t.Fatalf("the split did not get the entry's receipt: %q", bill.Attachment)
	}

	performJSONRequest[models.Entry](t, w.router, http.MethodPut, path, w.ownerToken,
		map[string]any{"title": "Late dinner", "attachment": receipt}, http.StatusOK)
	billBody := map[string]any{
		"entry_id": entry.ID, "title": "Late dinner", "total_amount": "500", "date": "2026-11-20", "attachment": receipt,
		"participants": []map[string]any{{"friend_id": friend.ID, "share_amount": "200", "direction": splitDirectionFriendOwesUser}},
	}
	performJSONRequest[models.SplitBill](t, w.router, http.MethodPut, fmt.Sprintf("/v1/split/bills/%d", bill.ID), w.ownerToken, billBody, http.StatusOK)

	// The same user's second record may use the same file.
	second := w.createEntry(t, w.ownerToken, receipt)

	billReceipt := w.upload(t)
	billBody["attachment"] = billReceipt
	performJSONRequest[models.SplitBill](t, w.router, http.MethodPut, fmt.Sprintf("/v1/split/bills/%d", bill.ID), w.ownerToken, billBody, http.StatusOK)
	var updated models.Entry
	if err := database.DB.First(&updated, entry.ID).Error; err != nil || updated.Attachment != billReceipt {
		t.Fatalf("the bill's new receipt did not reach the entry: %q %v", updated.Attachment, err)
	}

	photo := w.upload(t)
	group := performJSONRequest[models.SplitGroup](t, w.router, http.MethodPost, "/v1/split/groups", w.ownerToken,
		map[string]any{"name": "Goa", "kind": "trip", "photo_url": photo}, http.StatusCreated)
	performJSONRequest[models.SplitGroup](t, w.router, http.MethodPut, fmt.Sprintf("/v1/split/groups/%d", group.ID), w.ownerToken,
		map[string]any{"name": "Goa 2", "kind": "trip", "photo_url": photo}, http.StatusOK)
	performJSONRequest[models.Feedback](t, w.router, http.MethodPost, "/v1/feedback", w.ownerToken,
		map[string]any{"type": "bug", "impact": "high", "title": "t", "message": "m", "attachments": []string{w.upload(t)}}, http.StatusCreated)

	// Values that are not files on this server are stored as before.
	w.createEntry(t, w.memberToken, "https://example.com/photo.jpg")

	performJSONRequest[map[string]string](t, w.router, http.MethodDelete, fmt.Sprintf("/v1/entries/%d", second.ID), w.ownerToken, nil, http.StatusOK)
	performJSONRequest[map[string]string](t, w.router, http.MethodDelete, path, w.ownerToken, nil, http.StatusOK)
	if fileExists(receipt) || fileExists(billReceipt) {
		t.Fatalf("unused receipts were left behind: first=%v second=%v", fileExists(receipt), fileExists(billReceipt))
	}
	if !fileExists(photo) {
		t.Fatal("the group photo was removed")
	}
}
