package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/chhuax/craft/pkg/protocol"
)

// 评论和评审列表均按时间正序，首次定位到末页，之后通过页码向前读，避免为最近几条遍历全部历史。
func (e *extension) recent(ctx context.Context, path string, size, page int) ([]comment, int, error) {
	newest := page == 0
	if newest {
		page = 1
	}
	fetch := func(n int) ([]comment, int, error) {
		var rows []comment
		h, err := e.get(ctx, fmt.Sprintf("%s?per_page=%d&page=%d", path, size, n), &rows)
		if err != nil {
			return nil, 0, err
		}
		if len(rows) > size {
			return nil, 0, errors.New("GitHub 评论返回条数超限")
		}
		last, err := linkPage(h, "last")
		if newest && last == 0 && h.Get("Link") != "" {
			next, linkErr := linkPage(h, "next")
			if linkErr != nil || next != 0 {
				return nil, 0, errors.New("GitHub 未提供末页页码")
			}
		}
		return rows, last, err
	}
	rows, last, err := fetch(page)
	if err == nil && newest && last > page {
		page = last
		rows, _, err = fetch(page)
	}
	for i := range rows {
		rows[i].Body = clip(rows[i].Body, 4096)
	}
	return rows, page - 1, err
}

func (e *extension) readContext(ctx context.Context, p protocol.ContextReadParams) (any, error) {
	o, err := parseObject(p.Thread)
	if err != nil {
		return nil, err
	}
	if o.kind != "pull_request" {
		return nil, invalid("需要 PR 对象")
	}
	paths := map[string]string{"comments": o.path("issues") + "/comments", "reviews": o.endpoint() + "/reviews", "reviewComments": o.endpoint() + "/comments"}
	pages := map[string]int{"comments": 0, "reviews": 0, "reviewComments": 0}
	if len(p.Cursor) > 0 {
		pages = nil
		if json.Unmarshal(p.Cursor, &pages) != nil || len(pages) == 0 {
			return nil, invalid("无效的上下文游标")
		}
		for name, page := range pages {
			if paths[name] == "" || page < 1 {
				return nil, invalid("无效的上下文分页")
			}
		}
	}
	result := map[string]any{}
	if len(p.Cursor) == 0 {
		d, err := e.detail(ctx, o)
		if err != nil {
			return nil, err
		}
		result["title"], result["body"], result["state"] = d.Title, clip(d.Body, 8192), d.State
	}
	next := map[string]int{}
	for _, name := range []string{"comments", "reviewComments", "reviews"} {
		page, ok := pages[name]
		if !ok {
			continue
		}
		rows, prev, err := e.recent(ctx, paths[name], 10, page)
		if err != nil {
			return nil, err
		}
		items := make([]map[string]any, 0, len(rows))
		for _, c := range rows {
			item := map[string]any{"id": c.ID, "author": c.User.Login, "body": c.Body}
			if c.State != "" {
				item["state"] = c.State
			}
			at := c.Created
			if !c.Submitted.IsZero() {
				at = c.Submitted
			}
			if !at.IsZero() {
				item["at"] = millis(at)
			}
			items = append(items, item)
		}
		result[name] = items
		if prev > 0 {
			next[name] = prev
		}
	}
	if len(next) > 0 {
		result["cursor"] = next
	}
	return result, nil
}
