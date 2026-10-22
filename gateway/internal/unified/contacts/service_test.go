package contacts

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"callrelay/gateway/internal/unified/store"
)

func serviceStore(t *testing.T) *store.Store {
	t.Helper()
	database, err := store.Open(filepath.Join(t.TempDir(), "contacts.sqlite"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return database
}

func principal(t *testing.T, database *store.Store, key string) string {
	t.Helper()
	id, err := database.PrincipalForKey(context.Background(), key)
	if err != nil {
		t.Fatalf("principal %s: %v", key, err)
	}
	return id
}

func list(t *testing.T, service *Service, principalID, query string, limit int) store.ContactPage {
	t.Helper()
	page, err := service.List(context.Background(), principalID, query, limit)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	return page
}

func TestImportAndRepeatedImportMerge(t *testing.T) {
	ctx := context.Background()
	database := serviceStore(t)
	service := NewService(database)
	alice := principal(t, database, "key_alice")

	card := "BEGIN:VCARD\nVERSION:3.0\nFN:李雷\nTEL;TYPE=CELL:+86 139 0013 9000\nEND:VCARD\n"
	first, err := service.ImportVCard(ctx, alice, []byte(card), true)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if first.Created != 1 || first.Updated != 0 {
		t.Fatalf("first import: %+v", first)
	}
	second, err := service.ImportVCard(ctx, alice, []byte(card), true)
	if err != nil {
		t.Fatalf("reimport: %v", err)
	}
	if second.Created != 0 || second.Unchanged != 1 {
		t.Fatalf("repeated import must merge: %+v", second)
	}
	contacts := list(t, service, alice, "", 100).Contacts
	if len(contacts) != 1 || len(contacts[0].Phones) != 1 {
		t.Fatalf("duplicate contacts after repeated import: %+v", contacts)
	}

	updated := "BEGIN:VCARD\nVERSION:3.0\nFN:李雷\nTEL;TYPE=CELL:+86 139 0013 9000\nTEL;TYPE=HOME:010-66668888\nEMAIL:lilei@example.com\nEND:VCARD\n"
	third, err := service.ImportVCard(ctx, alice, []byte(updated), true)
	if err != nil {
		t.Fatalf("update import: %v", err)
	}
	if third.Updated != 1 {
		t.Fatalf("expected an update, got %+v", third)
	}
	contacts = list(t, service, alice, "", 100).Contacts
	if len(contacts[0].Phones) != 2 || len(contacts[0].Emails) != 1 {
		t.Fatalf("merged fields wrong: %+v", contacts[0])
	}
}

func TestPreviewDoesNotWrite(t *testing.T) {
	ctx := context.Background()
	database := serviceStore(t)
	service := NewService(database)
	alice := principal(t, database, "key_alice")
	card := "BEGIN:VCARD\nVERSION:3.0\nFN:预览\nTEL:13800000000\nEND:VCARD\n"
	result, err := service.ImportVCard(ctx, alice, []byte(card), false)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if !result.Preview || result.Created != 1 {
		t.Fatalf("preview result: %+v", result)
	}
	if contacts := list(t, service, alice, "", 100).Contacts; len(contacts) != 0 {
		t.Fatalf("preview must not persist: %+v", contacts)
	}
}

func TestPrincipalIsolationOnEveryOperation(t *testing.T) {
	ctx := context.Background()
	database := serviceStore(t)
	service := NewService(database)
	alice := principal(t, database, "key_alice")
	bob := principal(t, database, "key_bob")
	if alice == bob {
		t.Fatal("different keys must map to different principals")
	}
	cardA := "BEGIN:VCARD\nVERSION:3.0\nFN:爱丽丝\nTEL:13800000001\nEND:VCARD\n"
	cardB := "BEGIN:VCARD\nVERSION:3.0\nFN:鲍勃\nTEL:13800000002\nEND:VCARD\n"
	if _, err := service.ImportVCard(ctx, alice, []byte(cardA), true); err != nil {
		t.Fatalf("import a: %v", err)
	}
	bobResult, err := service.ImportVCard(ctx, bob, []byte(cardB), true)
	if err != nil {
		t.Fatalf("import b: %v", err)
	}
	bobContact, err := service.Store.GetContact(ctx, bob, bobResult.Entries[0].ContactID)
	if err != nil {
		t.Fatalf("get bob: %v", err)
	}

	// Alice cannot read Bob's contact by id, search it, edit or delete it.
	if _, err := service.Store.GetContact(ctx, alice, bobContact.ID); err != store.ErrNotFound {
		t.Fatalf("cross-principal get must fail, got %v", err)
	}
	if found := list(t, service, alice, "鲍勃", 100).Contacts; len(found) != 0 {
		t.Fatalf("cross-principal search leaked: %+v", found)
	}
	if found := list(t, service, alice, "13800000002", 100).Contacts; len(found) != 0 {
		t.Fatalf("cross-principal number search leaked: %+v", found)
	}
	edited := bobContact
	edited.PrincipalID = alice
	edited.DisplayName = "改名"
	if _, err := service.Update(ctx, alice, edited); err != store.ErrNotFound {
		t.Fatalf("cross-principal edit must fail, got %v", err)
	}
	if err := service.Delete(ctx, alice, bobContact.ID); err != store.ErrNotFound {
		t.Fatalf("cross-principal delete must fail, got %v", err)
	}
	exported, err := service.ExportVCard(ctx, alice)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if strings.Contains(exported, "鲍勃") || strings.Contains(exported, "13800000002") {
		t.Fatalf("cross-principal export leaked: %q", exported)
	}
	// Bob's contact is still there.
	if _, err := service.Store.GetContact(ctx, bob, bobContact.ID); err != nil {
		t.Fatalf("bob contact damaged: %v", err)
	}
}

func TestSyncIsUpsertOnlyAndShared(t *testing.T) {
	ctx := context.Background()
	database := serviceStore(t)
	service := NewService(database)
	alice := principal(t, database, "key_alice")

	batch := []SyncContact{
		{ClientRef: "ref-1", DisplayName: "张三", SearchKey: "zhangsan 9426", Phones: []Field{{Label: "CELL", Value: "+8613800138000"}}},
		{ClientRef: "ref-2", DisplayName: "李四", Phones: []Field{{Value: "13900139000"}}},
	}
	first, err := service.SyncContacts(ctx, alice, batch)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if first.Created != 2 || first.Revision == 0 {
		t.Fatalf("sync result: %+v", first)
	}
	second, err := service.SyncContacts(ctx, alice, batch[:1])
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if second.Unchanged != 1 {
		t.Fatalf("repeated sync must be idempotent: %+v", second)
	}
	page := list(t, service, alice, "", 100)
	if len(page.Contacts) != 2 {
		t.Fatalf("sync must not delete by omission: %+v", page.Contacts)
	}
	if page.Revision < first.Revision {
		t.Fatalf("revision must be monotonic: %d < %d", page.Revision, first.Revision)
	}
	found := list(t, service, alice, "zhangsan", 100).Contacts
	if len(found) != 1 || found[0].DisplayName != "张三" {
		t.Fatalf("pinyin search failed: %+v", found)
	}
}

func TestDeletedContactLeavesRevisionedTombstone(t *testing.T) {
	ctx := context.Background()
	database := serviceStore(t)
	service := NewService(database)
	alice := principal(t, database, "key_alice")
	if _, err := service.SyncContacts(ctx, alice, []SyncContact{{
		ClientRef: "ref-delete", DisplayName: "待删除", Phones: []Field{{Value: "13500000000"}},
	}}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	page := list(t, service, alice, "", 100)
	contactID := page.Contacts[0].ID
	before := page.Revision
	if err := service.Delete(ctx, alice, contactID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// The change feed reports the deletion with its own revision, so an
	// incremental client learns about it instead of keeping a stale copy.
	changes, err := service.Pull(ctx, alice, before, 100)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(changes.Contacts) != 1 || !changes.Contacts[0].Deleted {
		t.Fatalf("expected one deletion tombstone: %+v", changes.Contacts)
	}
	if changes.Contacts[0].Revision <= before || changes.HasMore {
		t.Fatalf("tombstone revision/cursor wrong: %+v", changes)
	}
	if changes.NextSince != changes.Revision {
		t.Fatalf("complete page must advance the cursor to the current revision: %+v", changes)
	}
	// Display queries never show tombstones.
	if visible := list(t, service, alice, "", 100).Contacts; len(visible) != 0 {
		t.Fatalf("deleted contact still visible: %+v", visible)
	}
	// A stale device re-uploading the same client ref must not resurrect it.
	resurrect, err := service.SyncContacts(ctx, alice, []SyncContact{{
		ClientRef: "ref-delete", DisplayName: "待删除", Phones: []Field{{Value: "13500000000"}},
	}})
	if err != nil {
		t.Fatalf("resurrect sync: %v", err)
	}
	if resurrect.Skipped != 1 || resurrect.Created != 0 {
		t.Fatalf("tombstoned contact was resurrected: %+v", resurrect)
	}
	if visible := list(t, service, alice, "", 100).Contacts; len(visible) != 0 {
		t.Fatalf("resurrected contact visible: %+v", visible)
	}
	// An explicit restore is the only path back.
	restored, err := service.SyncContacts(ctx, alice, []SyncContact{{
		ClientRef: "ref-delete", Restore: true, DisplayName: "恢复", Phones: []Field{{Value: "13500000000"}},
	}})
	if err != nil {
		t.Fatalf("restore sync: %v", err)
	}
	if restored.Created != 1 {
		t.Fatalf("explicit restore should recreate: %+v", restored)
	}
}

func TestPaginationNeverSilentlyDropsContacts(t *testing.T) {
	ctx := context.Background()
	database := serviceStore(t)
	service := NewService(database)
	alice := principal(t, database, "key_alice")
	batch := make([]SyncContact, 0, 7)
	for i := 0; i < 7; i++ {
		batch = append(batch, SyncContact{
			ClientRef: "ref-" + string(rune('a'+i)), DisplayName: "联系人" + string(rune('A'+i)),
			Phones: []Field{{Value: string(rune('1'+i)) + "3800000000"}},
		})
	}
	if _, err := service.SyncContacts(ctx, alice, batch); err != nil {
		t.Fatalf("sync: %v", err)
	}
	// Incremental pull pages by cursor until complete; every contact appears.
	var seen []string
	cursor := int64(0)
	for page := 0; page < 10; page++ {
		changes, err := service.Pull(ctx, alice, cursor, 2)
		if err != nil {
			t.Fatalf("pull: %v", err)
		}
		for _, contact := range changes.Contacts {
			seen = append(seen, contact.ID)
		}
		if !changes.HasMore {
			if changes.NextSince != changes.Revision {
				t.Fatalf("complete cursor should equal revision: %+v", changes)
			}
			break
		}
		if changes.NextSince <= cursor {
			t.Fatalf("cursor did not advance: %d -> %d", cursor, changes.NextSince)
		}
		cursor = changes.NextSince
	}
	if len(seen) != 7 {
		t.Fatalf("paged pull dropped contacts: %d of 7 (%v)", len(seen), seen)
	}
	// Display pages report truncation honestly instead of pretending complete.
	display := list(t, service, alice, "", 3)
	if !display.HasMore || len(display.Contacts) != 3 {
		t.Fatalf("display page must report truncation: %+v", display)
	}
}

func TestConcurrentSyncSamePrincipalDoesNotDuplicate(t *testing.T) {
	ctx := context.Background()
	database := serviceStore(t)
	service := NewService(database)
	alice := principal(t, database, "key_alice")
	batch := []SyncContact{{
		ClientRef: "ref-race", DisplayName: "并发联系人",
		Phones: []Field{{Label: "CELL", Value: "+86 135 0000 0001"}},
	}}
	const workers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := service.SyncContacts(ctx, alice, batch); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent sync: %v", err)
	}
	contacts := list(t, service, alice, "", 100).Contacts
	if len(contacts) != 1 {
		t.Fatalf("concurrent same-principal sync duplicated contacts: %d", len(contacts))
	}
}

func TestDeleteDuringApplyDoesNotResurrect(t *testing.T) {
	ctx := context.Background()
	database := serviceStore(t)
	service := NewService(database)
	alice := principal(t, database, "key_alice")
	if _, err := service.SyncContacts(ctx, alice, []SyncContact{{
		ClientRef: "ref-del", DisplayName: "删除竞态",
		Phones: []Field{{Label: "CELL", Value: "13500000002"}},
	}}); err != nil {
		t.Fatalf("seed sync: %v", err)
	}
	page := list(t, service, alice, "", 100)
	contactID := page.Contacts[0].ID

	// Simulate another surface deleting the contact after this batch was
	// planned but before it is applied (the lock cannot be held by that
	// surface, e.g. an operator/web request in a separate transaction).
	service.beforeApply = func() {
		service.beforeApply = nil
		if err := database.DeleteContact(ctx, alice, contactID); err != nil {
			t.Errorf("mid-plan delete: %v", err)
		}
	}
	incoming := []SyncContact{{
		ClientRef: "ref-del", DisplayName: "删除竞态",
		Phones: []Field{
			{Label: "CELL", Value: "13500000002"},
			{Label: "HOME", Value: "010-12345678"},
		},
	}}
	result, err := service.SyncContacts(ctx, alice, incoming)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result.Skipped != 1 || result.Created != 0 {
		t.Fatalf("deleted contact must be skipped, not recreated: %+v", result)
	}
	if contacts := list(t, service, alice, "", 100).Contacts; len(contacts) != 0 {
		t.Fatalf("deleted contact resurrected: %+v", contacts)
	}
}

func TestPrincipalForKeyConcurrentFirstUse(t *testing.T) {
	ctx := context.Background()
	database := serviceStore(t)
	const workers = 8
	results := make(chan string, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := database.PrincipalForKey(ctx, "key_race")
			if err != nil {
				results <- "error: " + err.Error()
				return
			}
			results <- id
		}()
	}
	wg.Wait()
	close(results)
	first := ""
	for result := range results {
		if strings.HasPrefix(result, "error:") {
			t.Fatal(result)
		}
		if first == "" {
			first = result
		} else if result != first {
			t.Fatalf("concurrent first use produced two principals: %s != %s", result, first)
		}
	}
	if first == "" {
		t.Fatal("no principal returned")
	}
	if bound, ok, err := database.PrincipalBinding(ctx, "key_race"); err != nil || !ok || bound != first {
		t.Fatalf("binding mismatch: %v %v %s vs %s", err, ok, bound, first)
	}
}

func TestKeyRotationPreservesPrincipal(t *testing.T) {
	ctx := context.Background()
	database := serviceStore(t)
	service := NewService(database)
	alice := principal(t, database, "key_alice")
	_ = principal(t, database, "key_bob")
	card := "BEGIN:VCARD\nVERSION:3.0\nFN:保留\nTEL:13700000000\nEND:VCARD\n"
	if _, err := service.ImportVCard(ctx, alice, []byte(card), true); err != nil {
		t.Fatalf("import: %v", err)
	}
	if err := database.RotatePrincipalKey(ctx, "key_alice", "key_alice_v2"); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	rotated, err := database.PrincipalForKey(ctx, "key_alice_v2")
	if err != nil {
		t.Fatalf("principal v2: %v", err)
	}
	if rotated != alice {
		t.Fatalf("rotation must preserve the principal: %s != %s", rotated, alice)
	}
	contacts := list(t, service, rotated, "", 100).Contacts
	if len(contacts) != 1 || contacts[0].DisplayName != "保留" {
		t.Fatalf("contacts lost across rotation: %+v", contacts)
	}
	// Rotating a key onto an unrelated principal that owns contacts must not
	// merge them.
	if err := database.RotatePrincipalKey(ctx, "key_alice", "key_bob"); err != store.ErrPrincipalConflict {
		t.Fatalf("unrelated key merge must be refused, got %v", err)
	}
}

func TestContactLimitsAndEscaping(t *testing.T) {
	ctx := context.Background()
	database := serviceStore(t)
	service := NewService(database)
	alice := principal(t, database, "key_alice")
	oversized := strings.Repeat("a", MaxImportBytes+10)
	if _, err := service.ImportVCard(ctx, alice, []byte(oversized), true); err == nil {
		t.Fatal("expected oversize import error")
	}
	card := "BEGIN:VCARD\nVERSION:3.0\nFN:逗号\\,分号\\;换行\\n结束\nTEL:13600000000\nEND:VCARD\n"
	result, err := service.ImportVCard(ctx, alice, []byte(card), true)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.Created != 1 {
		t.Fatalf("import result: %+v", result)
	}
	contacts := list(t, service, alice, "", 100).Contacts
	exported, err := service.ExportVCard(ctx, alice)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if !strings.Contains(exported, "逗号\\,分号\\;换行\\n结束") {
		t.Fatalf("export escaping wrong: %q", exported)
	}
	if contacts[0].DisplayName != "逗号,分号;换行\n结束" {
		t.Fatalf("stored display name wrong: %q", contacts[0].DisplayName)
	}
}
