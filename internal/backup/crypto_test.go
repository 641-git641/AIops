package backup

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestEncryptDecryptRoundtrip(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.dump")
	enc := filepath.Join(dir, "src.dump.aes")
	dec := filepath.Join(dir, "dec.dump")
	payload := bytes.Repeat([]byte("hello backup\x00\x01\x02"), 1000) // 含二进制
	if err := os.WriteFile(src, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	pass := []byte("my-passphrase")
	if err := EncryptFile(src, enc, pass); err != nil {
		t.Fatal(err)
	}
	encInfo, _ := os.Stat(enc)
	if encInfo.Size() <= int64(len(payload)) {
		t.Fatalf("encrypted smaller than plain: %d <= %d", encInfo.Size(), len(payload))
	}
	if err := DecryptFile(enc, dec, pass); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dec)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("decrypted mismatch")
	}
}

func TestDecryptWrongPassphrase(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "s.dump")
	enc := filepath.Join(dir, "s.dump.aes")
	if err := os.WriteFile(src, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EncryptFile(src, enc, []byte("right")); err != nil {
		t.Fatal(err)
	}
	if err := DecryptFile(enc, filepath.Join(dir, "x"), []byte("wrong")); err == nil {
		t.Fatal("wrong passphrase must fail")
	}
}

func TestDecryptTampered(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "s.dump")
	enc := filepath.Join(dir, "s.dump.aes")
	if err := os.WriteFile(src, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EncryptFile(src, enc, []byte("p")); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(enc)
	b[len(b)-1] ^= 0xff // 篡改密文尾部
	if err := os.WriteFile(enc, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := DecryptFile(enc, filepath.Join(dir, "x"), []byte("p")); err == nil {
		t.Fatal("tampered ciphertext must fail (GCM auth)")
	}
}

func TestDecryptNonEncrypted(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "plain")
	if err := os.WriteFile(f, []byte("not encrypted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := DecryptFile(f, filepath.Join(dir, "x"), []byte("p")); err == nil {
		t.Fatal("non-encrypted file must be rejected")
	}
}
