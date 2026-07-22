package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Steam 商店搜索代理：把用户输入的中英文游戏名转成 AppID 候选列表。
// 底层用 https://store.steampowered.com/api/storesearch 官方接口（免 key）。
// 注意：搜索出来的是游戏本体的 AppID，很多游戏的专用服务器 AppID 与本体不同
// （如 Valheim: 892970 游戏 / 896660 服务器；Palworld: 1621890 游戏 / 2394010 服务器）。
// 前端会给出提示，用户可手动修正 AppID。

var steamClient = &http.Client{Timeout: 8 * time.Second}

type steamSearchItem struct {
	AppID  int    `json:"appid"`
	Name   string `json:"name"`
	Icon   string `json:"icon"`
	Price  string `json:"price,omitempty"`
	StoreURL string `json:"storeUrl"`
}

// handleSteamSearch: GET /api/steam/search?q=xxx&lang=schinese
func (m *Manager) handleSteamSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, 200, []steamSearchItem{})
		return
	}
	lang := r.URL.Query().Get("lang")
	if lang == "" {
		lang = "schinese"
	}
	items := steamStoreSearch(q, lang)
	// 若中文搜索没结果、且输入疑似中文，追加一次英文搜索兜底
	if len(items) == 0 && containsCJK(q) && lang != "english" {
		items = steamStoreSearch(q, "english")
	}
	// 若输入是纯 AppID 数字，前端直接可用；这里额外返回一个 "AppID: xxx" 占位
	if id := parseAppID(q); id > 0 {
		found := false
		for _, it := range items {
			if it.AppID == id {
				found = true
				break
			}
		}
		if !found {
			items = append([]steamSearchItem{{
				AppID:    id,
				Name:     fmt.Sprintf("AppID %d（手填）", id),
				StoreURL: fmt.Sprintf("https://store.steampowered.com/app/%d/", id),
			}}, items...)
		}
	}
	writeJSON(w, 200, items)
}

func steamStoreSearch(term, lang string) []steamSearchItem {
	u := "https://store.steampowered.com/api/storesearch/?" + url.Values{
		"term": {term},
		"l":    {lang},
		"cc":   {"CN"},
	}.Encode()
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", "mcs-panel/1.0")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	resp, err := steamClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	var body struct {
		Items []struct {
			ID       int    `json:"id"`
			Name     string `json:"name"`
			TinyImg  string `json:"tiny_image"`
			Price    *struct {
				Final int `json:"final"`
			} `json:"price"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil
	}
	out := make([]steamSearchItem, 0, len(body.Items))
	for _, it := range body.Items {
		out = append(out, steamSearchItem{
			AppID:    it.ID,
			Name:     it.Name,
			Icon:     it.TinyImg,
			StoreURL: fmt.Sprintf("https://store.steampowered.com/app/%d/", it.ID),
		})
	}
	return out
}

func containsCJK(s string) bool {
	for _, r := range s {
		if (r >= 0x4E00 && r <= 0x9FFF) || (r >= 0x3400 && r <= 0x4DBF) {
			return true
		}
	}
	return false
}

func parseAppID(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
		if n > 2000000000 {
			return 0
		}
	}
	return n
}
