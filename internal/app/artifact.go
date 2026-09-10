package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DownloadSigner issues and verifies time-limited download links. The link
// carries its own capability signature, so a browser click does not need the
// static MCP bearer token.
type DownloadSigner struct {
	secret  string
	ttl     time.Duration
	baseURL string
	now     func() time.Time
}

func NewDownloadSigner(secret, baseURL string, ttl time.Duration) DownloadSigner {
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	return DownloadSigner{secret: secret, ttl: ttl, baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/")}
}

func (d DownloadSigner) nowTime() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

func (d DownloadSigner) sign(workspace, path string, exp int64) string {
	mac := hmac.New(sha256.New, []byte(d.secret))
	_, _ = fmt.Fprintf(mac, "%s\n%s\n%d", workspace, path, exp)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// URL returns the download link, its expiry, and an error when the signer is
// not configured. A relative link is returned when no base URL is configured;
// it is still valid behind a reverse proxy on the same origin.
func (d DownloadSigner) URL(workspace, path string) (string, time.Time, error) {
	if strings.TrimSpace(d.secret) == "" {
		return "", time.Time{}, errors.New("download links are not configured")
	}
	expires := d.nowTime().Add(d.ttl)
	exp := expires.Unix()
	query := url.Values{}
	query.Set("w", workspace)
	query.Set("p", path)
	query.Set("exp", strconv.FormatInt(exp, 10))
	query.Set("sig", d.sign(workspace, path, exp))
	return d.baseURL + "/download?" + query.Encode(), expires, nil
}

func (d DownloadSigner) verify(workspace, path, exp, sig string) error {
	if strings.TrimSpace(d.secret) == "" {
		return errors.New("download links are not configured")
	}
	deadline, err := strconv.ParseInt(exp, 10, 64)
	if err != nil {
		return errors.New("invalid download link")
	}
	if d.nowTime().Unix() >= deadline {
		return errors.New("download link has expired")
	}
	if !hmac.Equal([]byte(d.sign(workspace, path, deadline)), []byte(sig)) {
		return errors.New("invalid download link")
	}
	return nil
}

// DownloadHandler serves signed file downloads. It is mounted outside the MCP
// bearer middleware because the signature is the capability.
func (a *App) DownloadHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		query := r.URL.Query()
		workspace, path := query.Get("w"), query.Get("p")
		if err := a.downloads.verify(workspace, path, query.Get("exp"), query.Get("sig")); err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		root, err := a.files.workspace.Root(workspace)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		target, err := resolve(root, path, false)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		info, err := os.Stat(target)
		if err != nil || info.IsDir() {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		lock := a.access.Lock(workspace)
		lock.RLock()
		defer lock.RUnlock()
		file, err := os.Open(target)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		defer file.Close()
		contentType := mime.TypeByExtension(filepath.Ext(target))
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Disposition", "attachment; filename=\""+sanitizeDownloadName(filepath.Base(target))+"\"")
		http.ServeContent(w, r, filepath.Base(target), info.ModTime(), file)
	})
}

func sanitizeDownloadName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" {
		return "download"
	}
	return name
}
