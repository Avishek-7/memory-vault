package store

import (
	"errors"
	"strings"
	"testing"
)

func TestLimitsForPlanFailsClosed(t *testing.T) {
	for _, plan := range []string{"free", "builder", "team"} {
		if l := LimitsForPlan(plan); l.MaxMemories <= 0 {
			t.Errorf("plan %q has no memory cap", plan)
		}
	}
	// An unrecognised plan — a typo, or one added to the database before the
	// code knows about it — must not be handed unlimited storage.
	unknown := LimitsForPlan("enterprise-platinum")
	if unknown != DefaultPlanLimits["free"] {
		t.Errorf("unknown plan resolved to %+v, want the strictest built-in plan", unknown)
	}
}

// TestUnlimitedIsTheDefault is the compatibility guarantee: a Store built the
// way the CLI, the TUI, and every pre-quota caller builds one enforces
// nothing, so an existing self-hosted vault is unaffected by this feature.
func TestUnlimitedIsTheDefault(t *testing.T) {
	st := setupTenantDB(t)
	if l := st.Limits(); l != Unlimited {
		t.Errorf("a Store from Open enforces %+v, want no limits", l)
	}
	if st.TenantID() != BootstrapTenantID {
		t.Errorf("TenantID = %q, want the bootstrap tenant", st.TenantID())
	}

	// And a write goes through regardless of size.
	big := strings.Repeat("word ", 5000)
	if _, err := st.SaveMemory("quota-none", "big", big, DefaultSource, DefaultKind); err != nil {
		t.Fatalf("unlimited Store rejected a write: %v", err)
	}
	t.Cleanup(func() { st.DeleteMemory("quota-none", "big") })
}

func TestMemoryCountQuotaIsEnforced(t *testing.T) {
	st := setupTenantDB(t)
	makeTenant(t, st, tenantAID, "a@example.test")
	tenant := st.ForTenant(tenantAID).WithLimits(PlanLimits{MaxMemories: 3})
	t.Cleanup(func() {
		for _, n := range []string{"m1", "m2", "m3", "m4"} {
			tenant.DeleteMemory("quota", n)
		}
	})

	for _, n := range []string{"m1", "m2", "m3"} {
		if _, err := tenant.SaveMemory("quota", n, "content", DefaultSource, DefaultKind); err != nil {
			t.Fatalf("SaveMemory(%s) below the cap failed: %v", n, err)
		}
	}

	_, err := tenant.SaveMemory("quota", "m4", "content", DefaultSource, DefaultKind)
	var quota *QuotaError
	if !errors.As(err, &quota) {
		t.Fatalf("SaveMemory past the cap returned %v, want a *QuotaError", err)
	}
	if quota.Resource != "memories" || quota.Limit != 3 || quota.Current != 3 {
		t.Errorf("QuotaError = %+v, want memories 3/3", quota)
	}

	// The rejected write must leave nothing behind — a partial insert would
	// both corrupt the count and hand the tenant storage it was denied.
	if content, err := tenant.Reassemble("quota", "m4"); err != nil {
		t.Fatalf("Reassemble: %v", err)
	} else if content != "" {
		t.Errorf("the rejected write persisted %q", content)
	}
}

// TestOverwriteDoesNotConsumeExtraQuota covers the case the check placement
// exists for: replacing a memory when already at the cap must succeed,
// because the old rows are deleted in the same transaction before the count
// is taken. If the check ran before the delete, a tenant at its limit could
// never edit anything again.
func TestOverwriteDoesNotConsumeExtraQuota(t *testing.T) {
	st := setupTenantDB(t)
	makeTenant(t, st, tenantAID, "a@example.test")
	tenant := st.ForTenant(tenantAID).WithLimits(PlanLimits{MaxMemories: 2})
	t.Cleanup(func() {
		tenant.DeleteMemory("quota-ow", "a")
		tenant.DeleteMemory("quota-ow", "b")
	})

	for _, n := range []string{"a", "b"} {
		if _, err := tenant.SaveMemory("quota-ow", n, "original", DefaultSource, DefaultKind); err != nil {
			t.Fatalf("SaveMemory(%s): %v", n, err)
		}
	}
	if _, err := tenant.SaveMemory("quota-ow", "a", "replacement", DefaultSource, DefaultKind); err != nil {
		t.Fatalf("overwriting an existing memory at the cap was rejected: %v", err)
	}
	if got, _ := tenant.Reassemble("quota-ow", "a"); got != "replacement" {
		t.Errorf("after overwrite content = %q, want %q", got, "replacement")
	}
}

func TestStorageByteQuotaIsEnforced(t *testing.T) {
	st := setupTenantDB(t)
	makeTenant(t, st, tenantAID, "a@example.test")
	tenant := st.ForTenant(tenantAID).WithLimits(PlanLimits{MaxContentBytes: 200})
	t.Cleanup(func() {
		tenant.DeleteMemory("quota-bytes", "small")
		tenant.DeleteMemory("quota-bytes", "big")
	})

	if _, err := tenant.SaveMemory("quota-bytes", "small", strings.Repeat("a", 100), DefaultSource, DefaultKind); err != nil {
		t.Fatalf("write under the byte cap failed: %v", err)
	}

	_, err := tenant.SaveMemory("quota-bytes", "big", strings.Repeat("b", 500), DefaultSource, DefaultKind)
	var quota *QuotaError
	if !errors.As(err, &quota) {
		t.Fatalf("write past the byte cap returned %v, want a *QuotaError", err)
	}
	if quota.Resource != "storage" || quota.Limit != 200 {
		t.Errorf("QuotaError = %+v, want storage with limit 200", quota)
	}
}

func TestUsageReportsOnlyTheBoundTenant(t *testing.T) {
	st := setupTenantDB(t)
	makeTenant(t, st, tenantAID, "a@example.test")
	makeTenant(t, st, tenantBID, "b@example.test")
	a := st.ForTenant(tenantAID)
	b := st.ForTenant(tenantBID)
	t.Cleanup(func() {
		a.DeleteMemory("usage", "mine")
		b.DeleteMemory("usage", "theirs")
	})

	if _, err := a.SaveMemory("usage", "mine", strings.Repeat("a", 50), DefaultSource, DefaultKind); err != nil {
		t.Fatalf("SaveMemory(A): %v", err)
	}
	for i, n := range []string{"theirs"} {
		if _, err := b.SaveMemory("usage", n, strings.Repeat("b", 900), DefaultSource, DefaultKind); err != nil {
			t.Fatalf("SaveMemory(B,%d): %v", i, err)
		}
	}

	got, err := a.Usage()
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if got.Memories != 1 {
		t.Errorf("tenant A Usage.Memories = %d, want 1 — another tenant's rows are being counted against it", got.Memories)
	}
	if got.ContentBytes != 50 {
		t.Errorf("tenant A Usage.ContentBytes = %d, want 50", got.ContentBytes)
	}
}

// TestQuotaCountsAcrossSpaces proves the cap is per tenant rather than per
// space. Spaces are a namespace, not a billing boundary, so a tenant must not
// be able to sidestep its plan by spreading writes across new space names.
func TestQuotaCountsAcrossSpaces(t *testing.T) {
	st := setupTenantDB(t)
	makeTenant(t, st, tenantAID, "a@example.test")
	tenant := st.ForTenant(tenantAID).WithLimits(PlanLimits{MaxMemories: 2})
	t.Cleanup(func() {
		tenant.DeleteMemory("space-one", "m")
		tenant.DeleteMemory("space-two", "m")
		tenant.DeleteMemory("space-three", "m")
	})

	if _, err := tenant.SaveMemory("space-one", "m", "content", DefaultSource, DefaultKind); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if _, err := tenant.SaveMemory("space-two", "m", "content", DefaultSource, DefaultKind); err != nil {
		t.Fatalf("second write: %v", err)
	}
	_, err := tenant.SaveMemory("space-three", "m", "content", DefaultSource, DefaultKind)
	var quota *QuotaError
	if !errors.As(err, &quota) {
		t.Fatalf("a third space escaped the cap (%v); quotas must count the whole tenant", err)
	}
}

// TestQuotaMeasuresBytesNotCharacters guards a mismatch that silently let
// tenants past their cap: Postgres length() counts CHARACTERS while Go's
// len() counts BYTES, so every non-ASCII memory was undercounted on read and
// charged at full size on write. A tenant storing emoji or non-Latin script
// could exceed MaxContentBytes by the difference.
func TestQuotaMeasuresBytesNotCharacters(t *testing.T) {
	st := setupTenantDB(t)
	makeTenant(t, st, tenantAID, "a@example.test")
	tenant := st.ForTenant(tenantAID)
	t.Cleanup(func() { tenant.DeleteMemory("utf8", "multibyte") })

	// Every rune here is multibyte, so characters and bytes differ sharply.
	content := strings.Repeat("é", 100) // 100 characters, 200 bytes
	wantBytes := int64(len(content))
	if wantBytes != 200 {
		t.Fatalf("test fixture is wrong: %d bytes, want 200", wantBytes)
	}
	if _, err := tenant.SaveMemory("utf8", "multibyte", content, DefaultSource, DefaultKind); err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}

	usage, err := tenant.Usage()
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if usage.ContentBytes != wantBytes {
		t.Errorf("Usage.ContentBytes = %d for %d bytes of multibyte content — usage is being "+
			"measured in characters, so a tenant can exceed its byte quota", usage.ContentBytes, wantBytes)
	}
}

// TestByteQuotaCannotBeExceededWithMultibyteContent is the same bug seen from
// the enforcement side rather than the reporting side.
func TestByteQuotaCannotBeExceededWithMultibyteContent(t *testing.T) {
	st := setupTenantDB(t)
	makeTenant(t, st, tenantAID, "a@example.test")
	// 300-byte cap. Two 200-byte writes must not both fit.
	tenant := st.ForTenant(tenantAID).WithLimits(PlanLimits{MaxContentBytes: 300})
	t.Cleanup(func() {
		tenant.DeleteMemory("utf8-cap", "first")
		tenant.DeleteMemory("utf8-cap", "second")
	})

	content := strings.Repeat("é", 100) // 200 bytes
	if _, err := tenant.SaveMemory("utf8-cap", "first", content, DefaultSource, DefaultKind); err != nil {
		t.Fatalf("first 200-byte write under a 300-byte cap failed: %v", err)
	}
	_, err := tenant.SaveMemory("utf8-cap", "second", content, DefaultSource, DefaultKind)
	var quota *QuotaError
	if !errors.As(err, &quota) {
		t.Fatalf("a second 200-byte write fit under a 300-byte cap (%v); usage must be "+
			"counted in bytes, not characters", err)
	}
}

func TestCanonicalPlan(t *testing.T) {
	for _, plan := range []string{"free", "builder", "team"} {
		if got := CanonicalPlan(plan); got != plan {
			t.Errorf("CanonicalPlan(%q) = %q, want it unchanged", plan, got)
		}
	}
	// Anything unrecognised must collapse to the same name its limits come
	// from, so callers deriving config keys from it read real configuration.
	for _, plan := range []string{"typo", "", "TEAM", "enterprise"} {
		if got := CanonicalPlan(plan); got != FallbackPlan {
			t.Errorf("CanonicalPlan(%q) = %q, want %q", plan, got, FallbackPlan)
		}
	}
}

// TestImportOfAnExistingMemoryIsNotDoubleCharged covers the non-overwrite
// path, where the existing rows are NOT deleted before the quota check.
// Counting them alongside the incoming copy charged the same memory twice,
// which could fail an import of a backup the tenant already holds — and
// Import returns on the first error, so that aborted the whole import
// instead of skipping the duplicate.
func TestImportOfAnExistingMemoryIsNotDoubleCharged(t *testing.T) {
	st := setupTenantDB(t)
	makeTenant(t, st, tenantAID, "a@example.test")
	content := strings.Repeat("x", 100)
	// Room for exactly one copy plus a little slack — enough that the memory
	// fits, but not twice.
	tenant := st.ForTenant(tenantAID).WithLimits(PlanLimits{MaxMemories: 1, MaxContentBytes: 150})
	t.Cleanup(func() { tenant.DeleteMemory("imp", "existing") })

	if _, err := tenant.SaveMemory("imp", "existing", content, DefaultSource, DefaultKind); err != nil {
		t.Fatalf("initial write: %v", err)
	}

	// Re-importing the same memory without overwrite must skip it, not be
	// rejected for exceeding a quota it already fits inside.
	res, err := tenant.Import([]MemoryExport{{
		Name: "existing", Space: "imp", Source: DefaultSource, Kind: DefaultKind, Content: content,
	}}, "", "", false)
	if err != nil {
		t.Fatalf("re-importing an already-stored memory was rejected: %v", err)
	}
	if len(res.Skipped) != 1 {
		t.Errorf("Import skipped %v, want the single duplicate skipped", res.Skipped)
	}
}

// TestOverwriteAtTheByteCapStillWorks is the same exclusion seen from the
// overwrite side: a tenant whose storage is entirely consumed by one memory
// must still be able to replace it with content of a similar size.
func TestOverwriteAtTheByteCapStillWorks(t *testing.T) {
	st := setupTenantDB(t)
	makeTenant(t, st, tenantAID, "a@example.test")
	tenant := st.ForTenant(tenantAID).WithLimits(PlanLimits{MaxContentBytes: 120})
	t.Cleanup(func() { tenant.DeleteMemory("ow-cap", "only") })

	if _, err := tenant.SaveMemory("ow-cap", "only", strings.Repeat("a", 100), DefaultSource, DefaultKind); err != nil {
		t.Fatalf("initial write: %v", err)
	}
	if _, err := tenant.SaveMemory("ow-cap", "only", strings.Repeat("b", 100), DefaultSource, DefaultKind); err != nil {
		t.Fatalf("replacing a memory that consumes the whole quota was rejected: %v", err)
	}
	usage, err := tenant.Usage()
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if usage.ContentBytes != 100 {
		t.Errorf("after overwrite usage = %d bytes, want 100 — the replaced copy is still counted", usage.ContentBytes)
	}
}

func TestFallbackPlanExists(t *testing.T) {
	// init() panics if this is violated, so reaching the test at all proves
	// it; asserting explicitly documents why the init exists.
	if _, ok := DefaultPlanLimits[FallbackPlan]; !ok {
		t.Fatalf("FallbackPlan %q missing from DefaultPlanLimits", FallbackPlan)
	}
	if LimitsForPlan("nonexistent-plan") == Unlimited {
		t.Error("an unrecognised plan resolved to unlimited; the fallback must be the strictest plan")
	}
}

// TestImportOfALargerDuplicateIsSkippedNotRejected covers the gap my first
// fix left: excluding the existing copy from usage still charged the INCOMING
// payload, so importing a duplicate larger than the stored copy could trip
// MaxContentBytes for a write that will never happen. Import aborts on the
// first error, so one oversized duplicate took the whole restore down.
func TestImportOfALargerDuplicateIsSkippedNotRejected(t *testing.T) {
	st := setupTenantDB(t)
	makeTenant(t, st, tenantAID, "a@example.test")
	tenant := st.ForTenant(tenantAID).WithLimits(PlanLimits{MaxContentBytes: 300})
	t.Cleanup(func() {
		tenant.DeleteMemory("imp-big", "existing")
		tenant.DeleteMemory("imp-big", "fresh")
	})

	if _, err := tenant.SaveMemory("imp-big", "existing", strings.Repeat("a", 100), DefaultSource, DefaultKind); err != nil {
		t.Fatalf("initial write: %v", err)
	}

	// The incoming copy is far bigger than the cap allows on top of what is
	// stored — but it will be skipped, so it must not be charged at all.
	// A second, genuinely new memory follows it to prove the import as a
	// whole survives rather than aborting on the duplicate.
	res, err := tenant.Import([]MemoryExport{
		{Name: "existing", Space: "imp-big", Content: strings.Repeat("b", 5000)},
		{Name: "fresh", Space: "imp-big", Content: strings.Repeat("c", 100)},
	}, "", "", false)
	if err != nil {
		t.Fatalf("import containing an oversized duplicate was rejected: %v", err)
	}
	if len(res.Skipped) != 1 || len(res.Imported) != 1 {
		t.Errorf("Import imported=%v skipped=%v, want the duplicate skipped and the new one imported",
			res.Imported, res.Skipped)
	}
	// And the stored copy must be untouched by the skipped write.
	if got, _ := tenant.Reassemble("imp-big", "existing"); got != strings.Repeat("a", 100) {
		t.Errorf("the skipped duplicate modified the stored memory (len %d)", len(got))
	}
}
