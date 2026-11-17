// crypto.go — 敏感字段加密（AES-256-GCM）。
// 主密钥优先级：环境变量 MUSE_MASTER_KEY > 数据目录 key.bin（首次自动生成）。
package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// encPrefix 标识一段密文（base64 编码的 nonce+ciphertext）。
const encPrefix = "enc:"

// Crypto 持有 AES-256-GCM 实例。
type Crypto struct {
	gcm cipher.AEAD
}

// LoadMasterKey 获取 32 字节主密钥。
// 若 MUSE_MASTER_KEY 为合法 32 字节 hex 则直接使用；否则对其做 SHA-256。
// 都没有时，从 dataDir/key.bin 读取；不存在则生成并落盘（权限 0600）。
func LoadMasterKey(dataDir string) ([]byte, error) {
	if env := strings.TrimSpace(os.Getenv("MUSE_MASTER_KEY")); env != "" {
		if b, err := hex.DecodeString(env); err == nil && len(b) == 32 {
			return b, nil
		}
		sum := sha256.Sum256([]byte(env))
		return sum[:], nil
	}
	keyPath := filepath.Join(dataDir, "key.bin")
	if b, err := os.ReadFile(keyPath); err == nil {
		if len(b) == 32 {
			return b, nil
		}
		return nil, fmt.Errorf("密钥文件 %s 长度异常（%d 字节，应为 32）", keyPath, len(b))
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("读取密钥文件失败: %w", err)
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("生成随机密钥失败: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, b, 0o600); err != nil {
		return nil, fmt.Errorf("写入密钥文件失败: %w", err)
	}
	return b, nil
}

// NewCrypto 用主密钥构造加解密器。
func NewCrypto(key []byte) (*Crypto, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Crypto{gcm: gcm}, nil
}

// NewCryptoFromDataDir 一步完成密钥加载与加解密器构造。
func NewCryptoFromDataDir(dataDir string) (*Crypto, error) {
	key, err := LoadMasterKey(dataDir)
	if err != nil {
		return nil, err
	}
	return NewCrypto(key)
}

// Encrypt 加密明文，返回 "enc:<base64>"。空串原样返回。
func (c *Crypto) Encrypt(plain string) string {
	if plain == "" {
		return ""
	}
	nonce := make([]byte, c.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		// 随机数失败时不允许降级为明文存储。
		panic("crypto: 获取随机 nonce 失败: " + err.Error())
	}
	ct := c.gcm.Seal(nonce, nonce, []byte(plain), nil)
	return encPrefix + base64.StdEncoding.EncodeToString(ct)
}

// Decrypt 解密 "enc:<base64>" 密文；非 enc: 前缀的值视为明文原样返回（兼容旧数据/导入）。
func (c *Crypto) Decrypt(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	if !strings.HasPrefix(v, encPrefix) {
		return v, nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(v, encPrefix))
	if err != nil {
		return "", fmt.Errorf("密文 base64 解码失败: %w", err)
	}
	ns := c.gcm.NonceSize()
	if len(raw) < ns+16 {
		return "", errors.New("密文长度异常")
	}
	pt, err := c.gcm.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", errors.New("解密失败：主密钥不匹配或密文已损坏")
	}
	return string(pt), nil
}

// IsEncrypted 判断值是否为密文形态。
func IsEncrypted(v string) bool { return strings.HasPrefix(v, encPrefix) }
