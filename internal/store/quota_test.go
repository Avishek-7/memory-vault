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
