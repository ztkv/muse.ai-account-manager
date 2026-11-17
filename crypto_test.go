package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	c, err := NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	cases := []string{"hello", "P@ssw0rd 中文✓", "sk-abc123_very_long_key_000000", " ", "a"}
	for _, want := range cases {
		enc := c.Encrypt(want)
		if enc == want {
			t.Fatalf("密文不应等于明文: %s", want)
		}
		if !IsEncrypted(enc) {
			t.Fatalf("缺少 enc: 前缀")
		}
		got, err := c.Decrypt(enc)
		if err != nil || got != want {
			t.Fatalf("往返失败: want=%q got=%q err=%v", want, got, err)
		}
	}
	if c.Encrypt("") != "" {
		t.Fatal("空串应原样返回")
	}
}

func TestDecryptPlaintextPassthrough(t *testing.T) {
	c, _ := NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	got, err := c.Decrypt("plain-password")
	if err != nil || got != "plain-password" {
		t.Fatalf("明文应原样返回: got=%q err=%v", got, err)
	}
}

func TestDecryptWrongKeyFails(t *testing.T) {
	c1, _ := NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	c2, _ := NewCrypto([]byte("ffffffffffffffffffffffffffffffff"))
	enc := c1.Encrypt("secret")
	if _, err := c2.Decrypt(enc); err == nil {
		t.Fatal("错误密钥应解密失败")
	}
}

func TestLoadMasterKeyFromKeyfile(t *testing.T) {
	dir := t.TempDir()
	k1, err := LoadMasterKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := LoadMasterKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(k1) != string(k2) {
		t.Fatal("key.bin 应稳定复用")
	}
	if _, err := os.Stat(filepath.Join(dir, "key.bin")); err != nil {
		t.Fatal("key.bin 应已生成")
	}
}

func TestLoadMasterKeyFromEnv(t *testing.T) {
	t.Setenv("MUSE_MASTER_KEY", "my-secret-passphrase")
	k, err := LoadMasterKey(t.TempDir())
	if err != nil || len(k) != 32 {
		t.Fatalf("env 主密钥应为 32 字节: len=%d err=%v", len(k), err)
	}
}
