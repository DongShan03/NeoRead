package neoread

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

const nutstoreDAV = "https://dav.jianguoyun.com/dav/"
const syncChunkSize = 4 << 20
const syncManifestLimit = 64 << 20

type davError struct {
	Status int
	Retry  time.Duration
}

func (e *davError) Error() string {
	switch e.Status {
	case 401, 403:
		return "坚果云认证失败，请检查账号和第三方应用密码"
	case 429, 503:
		return "坚果云请求过于频繁，请稍后重试"
	case 507:
		return "坚果云空间不足，请清理空间后重试"
	}
	return fmt.Sprintf("坚果云请求失败（HTTP %d）", e.Status)
}

type davClient struct {
	base     *url.URL
	account  string
	password string
	http     *http.Client
	interval time.Duration
	last     *time.Time
	folders  map[string]bool
}

func newDAVClient(c webdavConfig) *davClient {
	base, _ := url.Parse(nutstoreDAV)
	base.Path += "NeoRead/" + c.Folder + "/"
	return &davClient{
		base: base, account: c.Account, password: c.Password,
		// Never forward an application password to a redirect destination.
		http: &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		// Nutstore's free tier permits 600 requests / 30 minutes.
		interval: 3100 * time.Millisecond, last: &time.Time{}, folders: map[string]bool{},
	}
}

func (d *davClient) request(ctx context.Context, method, name string, body []byte) (*http.Response, error) {
	if wait := time.Until(d.last.Add(d.interval)); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	*d.last = time.Now()
	u := *d.base
	u.Path += name
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("无法创建坚果云请求")
	}
	req.SetBasicAuth(d.account, d.password)
	if method == "PROPFIND" {
		req.Header.Set("Depth", "1")
		req.Header.Set("Content-Type", "application/xml")
	} else if method == "PUT" {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	res, err := d.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("无法连接坚果云，请检查网络或代理设置")
	}
	return res, nil
}

func davStatus(res *http.Response) error {
	retry := time.Minute
	if seconds, err := strconv.Atoi(res.Header.Get("Retry-After")); err == nil && seconds > 0 {
		retry = time.Duration(seconds) * time.Second
	} else if date, err := http.ParseTime(res.Header.Get("Retry-After")); err == nil && time.Until(date) > 0 {
		retry = time.Until(date)
	}
	return &davError{Status: res.StatusCode, Retry: retry}
}

func (d *davClient) mkdir(ctx context.Context, name string) error {
	folderKey := d.base.Path + name
	if d.folders[folderKey] {
		return nil
	}
	res, err := d.request(ctx, "MKCOL", name, nil)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 201 && res.StatusCode != 405 {
		return davStatus(res)
	}
	d.folders[folderKey] = true
	return nil
}

func (d *davClient) prepare(ctx context.Context) error {
	base := *d.base
	// Nutstore does not allow PUT into /dav itself; create the app folder first.
	d.base.Path = "/dav/"
	for _, part := range strings.Split(strings.TrimPrefix(base.Path, "/dav/"), "/") {
		if part == "" {
			continue
		}
		if err := d.mkdir(ctx, part); err != nil {
			d.base = &base
			return err
		}
		d.base.Path += part + "/"
	}
	d.base = &base
	for _, name := range []string{"devices", "objects"} {
		if err := d.mkdir(ctx, name); err != nil {
			return err
		}
	}
	return nil
}

type davDevice struct {
	Name string
	ETag string
}

func (d *davClient) devices(ctx context.Context) ([]davDevice, error) {
	res, err := d.request(ctx, "PROPFIND", "devices/", []byte(
		`<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:getetag/><d:resourcetype/></d:prop></d:propfind>`))
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 207 {
		return nil, davStatus(res)
	}
	var listing struct {
		Responses []struct {
			Href  string `xml:"href"`
			Props []struct {
				Status string `xml:"status"`
				Prop   struct {
					ETag string `xml:"getetag"`
				} `xml:"prop"`
			} `xml:"propstat"`
		} `xml:"response"`
	}
	if xml.NewDecoder(io.LimitReader(res.Body, 2<<20)).Decode(&listing) != nil {
		return nil, errors.New("坚果云返回了无效的设备列表")
	}
	// Only device manifests are listed, never the potentially large object folder.
	// Refuse a provider-truncated listing rather than interpreting omissions as deletions.
	if len(listing.Responses) >= 750 {
		return nil, errors.New("同步目录中的设备列表过大，请为书库选择独立目录")
	}
	result := []davDevice{}
	seen := map[string]bool{}
	for _, item := range listing.Responses {
		u, err := url.Parse(item.Href)
		if err != nil {
			return nil, errors.New("坚果云返回了无效的设备路径")
		}
		u = d.base.ResolveReference(u)
		if u.Host != d.base.Host || path.Dir(u.Path)+"/" != d.base.Path+"devices/" {
			continue
		}
		name := path.Base(u.Path)
		if !strings.HasSuffix(name, ".json") || !syncID(strings.TrimSuffix(name, ".json")) || seen[name] {
			continue
		}
		etag := ""
		for _, prop := range item.Props {
			if strings.Contains(prop.Status, " 200 ") {
				etag = prop.Prop.ETag
			}
		}
		seen[name] = true
		result = append(result, davDevice{Name: name, ETag: etag})
	}
	return result, nil
}

func (d *davClient) get(ctx context.Context, name string, limit int64) ([]byte, error) {
	res, err := d.request(ctx, "GET", name, nil)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, davStatus(res)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil || int64(len(body)) > limit {
		return nil, errors.New("坚果云文件未完整下载或超过同步格式上限")
	}
	return body, nil
}

func (d *davClient) put(ctx context.Context, name string, body []byte) error {
	res, err := d.request(ctx, "PUT", name, body)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 && res.StatusCode != 201 && res.StatusCode != 204 {
		return davStatus(res)
	}
	return nil
}
