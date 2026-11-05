// Package enrich annotates IP addresses with their network operator (ASN
// and organisation) from a local MMDB database, so path results say who
// runs each hop without any online lookups.
//
// The default database is DB-IP's free "IP to ASN Lite" (CC BY 4.0):
// reports that use it carry the attribution "IP to ASN data by DB-IP.com".
package enrich

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	"github.com/oschwald/maxminddb-golang/v2"
)

// Attribution must accompany results annotated from DB-IP data.
const Attribution = "IP to ASN data by DB-IP.com (CC BY 4.0)"

// FileName is the database file name in a data directory.
const FileName = "dbip-asn-lite.mmdb"

// DB is an open ASN database.
type DB struct {
	r *maxminddb.Reader
}

// Open opens a database file.
func Open(path string) (*DB, error) {
	r, err := maxminddb.Open(path)
	if err != nil {
		return nil, err
	}
	return &DB{r: r}, nil
}

// Close releases the database.
func (d *DB) Close() error { return d.r.Close() }

type record struct {
	ASN uint32 `maxminddb:"autonomous_system_number"`
	Org string `maxminddb:"autonomous_system_organization"`
}

// Lookup returns "AS<number>" and the organisation for an address. Private
// and unknown addresses return empty strings.
func (d *DB) Lookup(addr string) (asn, org string) {
	a, err := netip.ParseAddr(addr)
	if err != nil || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() {
		return "", ""
	}
	var rec record
	if err := d.r.Lookup(a.Unmap()).Decode(&rec); err != nil || rec.ASN == 0 {
		return "", ""
	}
	return fmt.Sprintf("AS%d", rec.ASN), rec.Org
}

// DefaultPaths lists where PathGauge looks for the database.
func DefaultPaths(dataDir string) []string {
	var out []string
	if dataDir != "" {
		out = append(out, filepath.Join(dataDir, FileName))
	}
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, filepath.Join(home, ".local", "share", "pathgauge", FileName))
	}
	return append(out, filepath.Join("/var/lib/pathgauge", FileName))
}

// OpenDefault opens the first database found in DefaultPaths (nil if none).
func OpenDefault(dataDir string) *DB {
	for _, p := range DefaultPaths(dataDir) {
		if db, err := Open(p); err == nil {
			return db
		}
	}
	return nil
}

// Update downloads the current month's DB-IP ASN Lite database (falling
// back to the previous month) and installs it atomically at dest.
func Update(ctx context.Context, dest string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil { //nolint:gosec // public data, read by the service user
		return "", err
	}
	now := time.Now().UTC()
	var lastErr error
	for _, t := range []time.Time{now, now.AddDate(0, -1, 0)} {
		url := fmt.Sprintf("https://download.db-ip.com/free/dbip-asn-lite-%s.mmdb.gz", t.Format("2006-01"))
		if err := download(ctx, url, dest); err != nil {
			lastErr = err
			continue
		}
		return url, nil
	}
	return "", lastErr
}

func download(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "pathgauge")
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".dbip-*.mmdb")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, io.LimitReader(zr, 512<<20)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Validate before installing.
	db, err := Open(tmp.Name())
	if err != nil {
		return errors.New("downloaded file is not a valid database: " + err.Error())
	}
	db.Close()
	if err := os.Chmod(tmp.Name(), 0o644); err != nil { //nolint:gosec // public data, read by the service user
		return err
	}
	return os.Rename(tmp.Name(), dest)
}
