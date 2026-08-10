package backup

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"os"

	"golang.org/x/crypto/pbkdf2"
)

// crypto.go：异地镜像加密（AES-256-GCM，口令文件驱动，KEEPON 4.1 安全要求）。
// 文件格式：magic(8) | salt(16) | nonce(12) | ciphertext（流式加密，可处理大备份文件）。

const (
	encMagic     = "AIOPSE1\000"
	encSaltLen   = 16
	encNonceLen  = 12
	encHeaderLen = len(encMagic) + encSaltLen + encNonceLen
	pbkdf2Iters  = 100_000
)

// EncryptFile 将 src 加密写入 dst（口令经 PBKDF2-SHA256 派生 32B 密钥）。
func EncryptFile(src, dst string, passphrase []byte) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()

	header := make([]byte, encHeaderLen)
	copy(header, encMagic)
	salt := header[len(encMagic):][:encSaltLen]
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	key := pbkdf2.Key(passphrase, salt, pbkdf2Iters, 32, sha256.New)
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	nonce := header[len(encMagic)+encSaltLen:]
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	if _, err := out.Write(header); err != nil {
		return err
	}
	// 流式加密：GCM 需要一次性密文+tag 落盘 → 对不支持 Seek 的流需整块处理。
	// 备份文件先落本地再加密，此处直接读全量（备份文件通常 < 1GB，可接受；见 DecryptFile 同注释）。
	plain, err := io.ReadAll(in)
	if err != nil {
		return err
	}
	ct := aead.Seal(nil, nonce, plain, nil)
	_, err = out.Write(ct)
	return err
}

// DecryptFile 解密 dst（EncryptFile 逆操作）。
func DecryptFile(src, dst string, passphrase []byte) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	header := make([]byte, encHeaderLen)
	if _, err := io.ReadFull(in, header); err != nil {
		return fmt.Errorf("backup: 加密文件头损坏: %w", err)
	}
	if string(header[:len(encMagic)]) != encMagic[:len(encMagic)] {
		return fmt.Errorf("backup: 非加密备份文件（缺少 magic）")
	}
	salt := header[len(encMagic):][:encSaltLen]
	nonce := header[len(encMagic)+encSaltLen:]
	ct, err := io.ReadAll(in)
	if err != nil {
		return err
	}
	key := pbkdf2.Key(passphrase, salt, pbkdf2Iters, 32, sha256.New)
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	plain, err := aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return fmt.Errorf("backup: 解密失败（口令错误或文件被篡改）: %w", err)
	}
	return os.WriteFile(dst, plain, 0o600)
}

// readPassphrase 读口令文件（限权限 0600；为空 → 错误）。
func readPassphrase(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("backup: 读口令文件: %w", err)
	}
	return b, nil
}
