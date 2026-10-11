// Package objstore copies database backup dumps to an S3-compatible bucket and
// back (ADR 0016, section 2 and pass D export). It is the client only: the
// callers decide what to record and when to delete local files.
//
// Credentials are kept in memory. They are never logged, never placed in an
// error, and Credentials prints as a redacted value.
package objstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Config names the bucket and the endpoint. Insecure selects plain HTTP and is
// for tests and local MinIO only.
type Config struct {
	Endpoint string // host[:port], no scheme
	Bucket   string
	Prefix   string // optional key prefix, for example "backups"
	Region   string // optional; us-east-1 when empty
	Insecure bool
	Log      *slog.Logger // optional; nil discards logs
}

// Credentials is the access key pair from the Secret named by credentialsSecret.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
}

// String and GoString hide both values, so a logged or printed Credentials
// shows no secret.
func (Credentials) String() string   { return "objstore.Credentials{redacted}" }
func (Credentials) GoString() string { return "objstore.Credentials{redacted}" }

// Client is a bucket handle with a fixed prefix.
type Client struct {
	mc     *minio.Client
	bucket string
	prefix string
	log    *slog.Logger
}

// New builds a Client. It does not contact the endpoint.
func New(cfg Config, creds Credentials) (*Client, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, errors.New("objstore: endpoint and bucket are required")
	}
	if creds.AccessKeyID == "" || creds.SecretAccessKey == "" {
		return nil, errors.New("objstore: credentials need accessKeyId and secretAccessKey")
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	mc, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(creds.AccessKeyID, creds.SecretAccessKey, ""),
		Secure:       !cfg.Insecure,
		Region:       region,
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		// The endpoint error does not contain the credentials.
		return nil, fmt.Errorf("objstore: endpoint: %w", err)
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Client{
		mc:     mc,
		bucket: cfg.Bucket,
		prefix: strings.Trim(cfg.Prefix, "/"),
		log:    log,
	}, nil
}

// Key is the object key of a backup: <prefix>/<db>/<backupID>.dump. Names with
// a slash or a dot-dot element are refused, so a key cannot leave its folder.
func (c *Client) Key(db, backupID string) (string, error) {
	for _, part := range []string{db, backupID} {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, `/\`) {
			return "", fmt.Errorf("objstore: %q is not a valid key part", part)
		}
	}
	k := path.Join(db, backupID+".dump")
	if c.prefix != "" {
		k = c.prefix + "/" + k
	}
	return k, nil
}

// Upload sends the local file to key. The local file is not changed or removed,
// on success or failure.
func (c *Client) Upload(ctx context.Context, file, key string) error {
	f, err := os.Open(file)
	if err != nil {
		return fmt.Errorf("objstore: open %s: %w", file, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("objstore: stat %s: %w", file, err)
	}
	_, err = c.mc.PutObject(ctx, c.bucket, key, f, st.Size(), minio.PutObjectOptions{
		ContentType: "application/octet-stream",
	})
	if err != nil {
		c.log.Warn("backup upload failed", "key", key, "err", err)
		return fmt.Errorf("objstore: upload %s: %w", key, err)
	}
	c.log.Info("backup uploaded", "key", key, "bytes", st.Size())
	return nil
}

// Download fetches key into dest and checks its SHA-256 against wantSHA. On a
// mismatch or a read error, dest is removed and an error is returned.
func (c *Client) Download(ctx context.Context, key, dest, wantSHA string) error {
	obj, err := c.mc.GetObject(ctx, c.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return fmt.Errorf("objstore: get %s: %w", key, err)
	}
	defer obj.Close()
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("objstore: create %s: %w", dest, err)
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), obj)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(dest)
		return fmt.Errorf("objstore: download %s: %w", key, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSHA {
		_ = os.Remove(dest)
		return fmt.Errorf("objstore: sha256 of %s is %s, the backup recorded %s", key, got, wantSHA)
	}
	return nil
}

// Exists reports whether key is in the bucket. A missing key is not an error.
func (c *Client) Exists(ctx context.Context, key string) (bool, error) {
	_, err := c.mc.StatObject(ctx, c.bucket, key, minio.StatObjectOptions{})
	if err == nil {
		return true, nil
	}
	if resp := minio.ToErrorResponse(err); resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	return false, fmt.Errorf("objstore: stat %s: %w", key, err)
}

// Delete removes key. Deleting a missing key is not an error, so a retried
// retention pass can run again.
func (c *Client) Delete(ctx context.Context, key string) error {
	err := c.mc.RemoveObject(ctx, c.bucket, key, minio.RemoveObjectOptions{})
	if err == nil {
		c.log.Info("backup deleted", "key", key)
		return nil
	}
	if resp := minio.ToErrorResponse(err); resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("objstore: delete %s: %w", key, err)
}
