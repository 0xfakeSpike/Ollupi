package redact

import (
	"strings"
	"testing"
)

func TestMaskPhone(t *testing.T) {
	r := New()
	got := r.Mask("请拨打 13800000001，稍后回电")
	if strings.Contains(got, "13800000001") {
		t.Fatalf("phone leaked through: %q", got)
	}
	if !strings.Contains(got, "<PHONE:") {
		t.Fatalf("expected a PHONE placeholder, got %q", got)
	}
}

func TestMaskIDCard(t *testing.T) {
	r := New()
	got := r.Mask("身份证 11010519491231002X 已登记")
	if strings.Contains(got, "11010519491231002X") {
		t.Fatalf("id card leaked through: %q", got)
	}
	if !strings.Contains(got, "<IDCARD:") {
		t.Fatalf("expected an IDCARD placeholder, got %q", got)
	}
}

func TestMaskBankCard(t *testing.T) {
	r := New()
	got := r.Mask("卡号 4242424242424242 有效")
	if strings.Contains(got, "4242424242424242") {
		t.Fatalf("bank card leaked through: %q", got)
	}
	if !strings.Contains(got, "<BANKCARD:") {
		t.Fatalf("expected a BANKCARD placeholder, got %q", got)
	}
}

func TestMaskEmail(t *testing.T) {
	r := New()
	got := r.Mask("联系 a.b-c@example.com 即可")
	if strings.Contains(got, "a.b-c@example.com") {
		t.Fatalf("email leaked through: %q", got)
	}
	if !strings.Contains(got, "<EMAIL:") {
		t.Fatalf("expected an EMAIL placeholder, got %q", got)
	}
}

func TestMaskSecrets(t *testing.T) {
	r := New()
	got := r.Mask("key=sk-abcdefghijklmnopqrstuvwxyz123456 aws=AKIAIOSFODNN7EXAMPLE")
	for _, secret := range []string{"sk-abcdefghijklmnopqrstuvwxyz123456", "AKIAIOSFODNN7EXAMPLE"} {
		if strings.Contains(got, secret) {
			t.Fatalf("secret leaked through: %q", got)
		}
	}
	if !strings.Contains(got, "<SECRET:") {
		t.Fatalf("expected a SECRET placeholder, got %q", got)
	}
}

func TestMaskPrivateKeyBlock(t *testing.T) {
	r := New()
	got := r.Mask("key: -----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBgkqhkiG9w0BAQ\n-----END PRIVATE KEY-----")
	if strings.Contains(got, "MIIEvQIBADANBgkqhkiG9w0BAQ") {
		t.Fatalf("private key leaked through: %q", got)
	}
	if !strings.Contains(got, "<SECRET:") {
		t.Fatalf("expected a SECRET placeholder, got %q", got)
	}
}

func TestMaskJWT(t *testing.T) {
	r := New()
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"
	got := r.Mask("token " + jwt)
	if strings.Contains(got, jwt) {
		t.Fatalf("jwt leaked through: %q", got)
	}
	if !strings.Contains(got, "<JWT:") {
		t.Fatalf("expected a JWT placeholder, got %q", got)
	}
}

func TestDeterministicAcrossInstances(t *testing.T) {
	a, b := New(), New()
	gotA, gotB := a.Mask("13800000001"), b.Mask("13800000001")
	if gotA != gotB {
		t.Fatalf("placeholder differs across instances: %q vs %q", gotA, gotB)
	}
}

func TestRestoreRoundTrip(t *testing.T) {
	r := New()
	src := "电话 13800000001，邮箱 a@b.com"
	masked := r.Mask(src)
	if masked == src {
		t.Fatal("mask did not change the text")
	}
	if got := r.Restore(masked); got != src {
		t.Fatalf("restore mismatch:\n got %q\nwant %q", got, src)
	}
}

func TestRestoreValueNested(t *testing.T) {
	r := New()
	phone := r.Mask("13800000001")
	args := map[string]any{
		"phone":  phone,
		"list":   []any{phone},
		"nested": map[string]any{"tel": phone},
	}
	r.RestoreValue(args)
	if args["phone"] != "13800000001" {
		t.Fatalf("top-level phone not restored: %v", args["phone"])
	}
	if args["list"].([]any)[0] != "13800000001" {
		t.Fatalf("list phone not restored: %v", args["list"])
	}
	if args["nested"].(map[string]any)["tel"] != "13800000001" {
		t.Fatalf("nested phone not restored: %v", args["nested"])
	}
}

func TestIDCardBeatsBankCardOnOverlap(t *testing.T) {
	// 18 digits that pass Luhn but fail the ID checksum: they must be treated
	// as a bank card, proving the validators disambiguate the overlap.
	r := New()
	got := r.Mask("123456789012345671")
	if !strings.Contains(got, "<BANKCARD:") {
		t.Fatalf("expected BANKCARD for a Luhn-valid non-ID 18-digit number, got %q", got)
	}
	if strings.Contains(got, "<IDCARD:") {
		t.Fatalf("18-digit number wrongly classified as IDCARD: %q", got)
	}
}

func TestNonSensitiveUnchanged(t *testing.T) {
	r := New()
	for _, s := range []string{
		"普通文本没有任何敏感值",
		"编号 01234567891", // 11 digits but not a mobile prefix
		"版本 12.3.4",      // three-part version, not a dotted quad
	} {
		if got := r.Mask(s); got != s {
			t.Fatalf("text unexpectedly masked: %q -> %q", s, got)
		}
	}
}
