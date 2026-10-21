package qoder

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"regexp"
	"strings"
)

// attachments.go 附件处理，移植自 9router open-sse/shared/qoder/attachments.js。
//
// 官方 qodercli 不会把图片/PDF 字节塞进 agent_chat_generation，而是先 PUT 到
// /algo/api/v2/image/upload（COSY 签名的 multipart），再把返回的 OSS URL 放进
// 消息里。Claude Code / OpenAI SDK 习惯内联 data URI，直传会让 10MB 图片变成
// 30MB+ JSON，上游 413（模型窗口才 ~200k token）。
//
// 这里做三件事：
//  1. 内联图片上传到 Qoder 文件接口（同请求内按 sha256 去重）；
//  2. 过大的非图片文件块替换成一句占位文本；
//  3. 兜底压缩残留的 data URI（上传失败且体积小 → 保留内联）。

// Uploader 图片上传器（relay 侧注入带代理/超时的实现；nil 则退化为内联或占位）。
type Uploader interface {
	Upload(ctx context.Context, data []byte, mediaType string) (string, error)
}

// 图片 MIME 与 data URI 识别。
var (
	imageMimeRe = regexp.MustCompile(`^image/`)
	dataURIRe   = regexp.MustCompile(`data:[^;]+;base64,[A-Za-z0-9+/=\s]+`)
)

// dataURI 解析结果。
type dataURI struct {
	mimeType string
	base64   string
}

// parseDataURI 解析 "data:<mime>;base64,<b64>"；非法返回 false。
func parseDataURI(s string) (dataURI, bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "data:") {
		return dataURI{}, false
	}
	rest := s[len("data:"):]
	idx := strings.Index(rest, ";base64,")
	if idx < 0 {
		return dataURI{}, false
	}
	mimeType := rest[:idx]
	b64 := rest[idx+len(";base64,"):]
	if b64 == "" {
		return dataURI{}, false
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	return dataURI{mimeType: mimeType, base64: b64}, true
}

// mimeExt MIME → 文件扩展名（上传文件名用）。
func mimeExt(mime string) string {
	m := strings.ToLower(mime)
	switch {
	case strings.Contains(m, "png"):
		return "png"
	case strings.Contains(m, "jpeg"), strings.Contains(m, "jpg"):
		return "jpg"
	case strings.Contains(m, "gif"):
		return "gif"
	case strings.Contains(m, "webp"):
		return "webp"
	case strings.Contains(m, "bmp"):
		return "bmp"
	case strings.Contains(m, "pdf"):
		return "pdf"
	}
	return "bin"
}

// decodedBytes base64 解码后的大致字节数（不实际解码）。
func decodedBytes(b64 string) int {
	compact := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
			return -1
		}
		return r
	}, b64)
	return len(compact) * 3 / 4
}

// stubText 附件占位文本（与 9router 文案一致，便于对照排查）。
func stubText(name, mime string, bytes int, reason string) string {
	label := name
	if label == "" {
		label = mime
	}
	if label == "" {
		label = "attachment"
	}
	size := ""
	if bytes > 0 {
		size = fmt.Sprintf(", %d bytes", bytes)
	}
	return fmt.Sprintf("[file omitted: %s%s — %s]", label, size, reason)
}

// imageURLBlock OpenAI 图片块。
func imageURLBlock(url string) map[string]any {
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}
}

// textBlock OpenAI 文本块。
func textBlock(text string) map[string]any {
	return map[string]any{"type": "text", "text": text}
}

// rewriteCtx 一次请求内的附件重写上下文。
type rewriteCtx struct {
	ctx      context.Context
	rc       Resolved
	uploader Uploader
	cache    map[string]string // sha256(data) → OSS URL（同请求内去重）
}

// uploadResult 单个附件处理结果。
type uploadResult struct {
	url   string // 上传成功
	keep  bool   // 保留原内联
	stub  bool   // 改占位
	bytes int
	mime  string
}

// uploadInline 处理一个内联附件：上传 → 保留内联 → 占位。
func (c *rewriteCtx) uploadInline(base64Data, mime string) uploadResult {
	compact := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
			return -1
		}
		return r
	}, base64Data)
	if compact == "" {
		return uploadResult{}
	}
	size := decodedBytes(compact)
	if size > MaxImageBytes {
		return uploadResult{stub: true, bytes: size, mime: mime}
	}
	data, err := base64.StdEncoding.DecodeString(compact)
	if err != nil {
		return uploadResult{stub: true, bytes: size, mime: mime}
	}
	digest := sha256Hex(data)
	if c.cache != nil {
		if url, ok := c.cache[digest]; ok {
			return uploadResult{url: url, bytes: size, mime: mime}
		}
	}
	if c.uploader != nil {
		url, err := c.uploader.Upload(c.ctx, data, mime)
		if err == nil && url != "" {
			if c.cache != nil {
				c.cache[digest] = url
			}
			return uploadResult{url: url, bytes: size, mime: mime}
		}
	}
	if size <= InlineFallbackMaxBytes {
		return uploadResult{keep: true, bytes: size, mime: mime}
	}
	return uploadResult{stub: true, bytes: size, mime: mime}
}

// rewriteBlock 重写单个 content block；返回 nil 表示丢弃该块。
func (c *rewriteCtx) rewriteBlock(block map[string]any) map[string]any {
	switch block["type"] {
	case "image_url":
		raw := ""
		switch v := block["image_url"].(type) {
		case string:
			raw = v
		case map[string]any:
			raw, _ = v["url"].(string)
		}
		if raw == "" {
			return nil
		}
		if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
			return imageURLBlock(raw)
		}
		parsed, ok := parseDataURI(raw)
		if !ok {
			return textBlock(stubText("attachment", "", 0, "unreadable data URI"))
		}
		if !imageMimeRe.MatchString(parsed.mimeType) {
			return textBlock(stubText("file", parsed.mimeType, decodedBytes(parsed.base64),
				"non-image bytes are not inlined into Qoder context"))
		}
		up := c.uploadInline(parsed.base64, parsed.mimeType)
		switch {
		case up.url != "":
			return imageURLBlock(up.url)
		case up.keep:
			return imageURLBlock(raw)
		default:
			return textBlock(stubText("image", parsed.mimeType, up.bytes, "upload failed; not inlined"))
		}

	case "image":
		src, _ := block["source"].(map[string]any)
		if src == nil {
			return block
		}
		if src["type"] == "url" {
			if u, _ := src["url"].(string); u != "" {
				return imageURLBlock(u)
			}
		}
		if src["type"] == "base64" {
			data, _ := src["data"].(string)
			mime, _ := src["media_type"].(string)
			if mime == "" {
				mime = "image/png"
			}
			up := c.uploadInline(data, mime)
			switch {
			case up.url != "":
				return imageURLBlock(up.url)
			case up.keep:
				return imageURLBlock("data:" + mime + ";base64," + data)
			default:
				return textBlock(stubText("image", mime, up.bytes, "upload failed; not inlined"))
			}
		}

	case "file":
		file, _ := block["file"].(map[string]any)
		name := "file"
		mime := "application/octet-stream"
		b64 := ""
		if file != nil {
			for _, k := range []string{"filename", "name"} {
				if v, ok := file[k].(string); ok && v != "" {
					name = v
					break
				}
			}
			if v, ok := file["format"].(string); ok && v != "" {
				mime = v
			}
			if v, ok := file["file_data"].(string); ok {
				if parsed, ok := parseDataURI(v); ok {
					b64, mime = parsed.base64, parsed.mimeType
				} else if v != "" {
					b64 = v
				}
			}
		}
		if b64 != "" && imageMimeRe.MatchString(mime) {
			if up := c.uploadInline(b64, mime); up.url != "" {
				return imageURLBlock(up.url)
			}
		}
		return textBlock(stubText(name, mime, decodedBytes(b64),
			"Qoder reads documents via its file API, not inlined bytes"))

	case "document":
		src, _ := block["source"].(map[string]any)
		name := "document"
		if v, ok := block["title"].(string); ok && v != "" {
			name = v
		}
		if src == nil {
			return block
		}
		if src["type"] == "base64" {
			data, _ := src["data"].(string)
			mime, _ := src["media_type"].(string)
			if mime == "" {
				mime = "application/pdf"
			}
			if imageMimeRe.MatchString(mime) {
				if up := c.uploadInline(data, mime); up.url != "" {
					return imageURLBlock(up.url)
				}
			}
			return textBlock(stubText(name, mime, decodedBytes(data),
				"Qoder reads documents via its file API, not inlined bytes"))
		}
	}
	return block
}

// stripLargeDataURIs 压缩文本里超大的内联 data URI（保留小的）。
func stripLargeDataURIs(text string) string {
	if !strings.Contains(text, "data:") || len(text) <= 8192 {
		return text
	}
	return dataURIRe.ReplaceAllStringFunc(text, func(m string) string {
		parsed, ok := parseDataURI(strings.TrimSpace(m))
		size := len(m)
		mime := ""
		if ok {
			size = decodedBytes(parsed.base64)
			mime = parsed.mimeType
		}
		if size <= InlineFallbackMaxBytes {
			return m
		}
		return stubText("", mime, size, "inlined data URI stripped from Qoder context")
	})
}

// rewriteContent 重写一条 message 的 content。
func (c *rewriteCtx) rewriteContent(content any) any {
	if s, ok := content.(string); ok {
		return stripLargeDataURIs(s)
	}
	list, ok := content.([]any)
	if !ok {
		return content
	}
	out := make([]any, 0, len(list))
	for _, raw := range list {
		block, ok := raw.(map[string]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		if next := c.rewriteBlock(block); next != nil {
			out = append(out, next)
		}
	}
	if len(out) == 0 {
		return ""
	}
	return out
}

// RewriteAttachments 就地重写 messages 里的附件（输出重写后的 messages 副本）。
// 返回 (重写后的 messages, 是否发生了上传/占位)。
func RewriteAttachments(ctx context.Context, messages []any, rc Resolved, uploader Uploader) []any {
	c := &rewriteCtx{ctx: ctx, rc: rc, uploader: uploader, cache: map[string]string{}}
	out := make([]any, 0, len(messages))
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		// Ollama 风格 msg.images 侧挂字段：折进 content 后再重写。
		if imgs, ok := msg["images"].([]any); ok {
			extras := make([]any, 0, len(imgs))
			for _, u := range imgs {
				extras = append(extras, imageURLBlock(fmt.Sprint(u)))
			}
			if list, ok := msg["content"].([]any); ok {
				msg["content"] = append(list, extras...)
			} else {
				text := ""
				if s, ok := msg["content"].(string); ok {
					text = s
				}
				merged := []any{textBlock(text)}
				msg["content"] = append(merged, extras...)
			}
			delete(msg, "images")
		}
		msg["content"] = c.rewriteContent(msg["content"])
		out = append(out, msg)
	}
	return out
}

// payloadSize messages 序列化后的字节数（超限判定用）。
func payloadSize(messages []any) int {
	raw, err := jsonMarshal(messages)
	if err != nil {
		return 0
	}
	return len(raw)
}

// StripRemainingDataURIs payload 仍超预算时，把残留的 data URI 全部替换成占位。
func StripRemainingDataURIs(messages []any) {
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch content := msg["content"].(type) {
		case string:
			if strings.Contains(content, "data:") {
				msg["content"] = dataURIRe.ReplaceAllStringFunc(content, func(m string) string {
					return stubText("", "", len(m), "payload over Qoder size budget")
				})
			}
		case []any:
			next := make([]any, 0, len(content))
			for _, rraw := range content {
				block, ok := rraw.(map[string]any)
				if !ok {
					next = append(next, rraw)
					continue
				}
				if block["type"] == "image_url" {
					url := ""
					switch v := block["image_url"].(type) {
					case string:
						url = v
					case map[string]any:
						url, _ = v["url"].(string)
					}
					if strings.HasPrefix(url, "data:") {
						next = append(next, textBlock(stubText("image", "", 0, "payload over Qoder size budget")))
						continue
					}
				}
				if text, ok := block["text"].(string); ok && strings.Contains(text, "data:") {
					block["text"] = dataURIRe.ReplaceAllStringFunc(text, func(m string) string {
						return stubText("", "", len(m), "payload over Qoder size budget")
					})
				}
				next = append(next, block)
			}
			msg["content"] = next
		}
	}
}

// ---- 上传实现 ----

// HTTPUploader 用渠道 HTTP client（含代理/超时）上传图片。
type HTTPUploader struct {
	Client Doer
	RC     Resolved
}

// Upload PUT {chatBase}/algo/api/v2/image/upload（multipart，COSY 签名）。
func (u HTTPUploader) Upload(ctx context.Context, data []byte, mediaType string) (string, error) {
	client := u.Client
	client = doerOrDefault(client)
	requestID := UUID()
	target := ImageUploadURL(u.RC.Region, u.RC.Token, requestID)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	filename := "image." + mimeExt(mediaType)
	h := make(map[string][]string)
	h["Content-Disposition"] = []string{fmt.Sprintf(`form-data; name="file"; filename="%s"`, filename)}
	h["Content-Type"] = []string{mediaType}
	part, err := mw.CreatePart(h)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(data); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}
	body := buf.Bytes()

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	hdr, err := BuildHeaders(body, target, u.RC.Creds())
	if err != nil {
		return "", err
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
	req.Header.Set("AI-CLIENT-TIMESTAMP", fmt.Sprintf("%d", nowUnix()))
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode != http.StatusOK {
		return "", &UpstreamError{Status: resp.StatusCode, Body: truncate(string(raw), 180)}
	}
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("qoder image upload: parse response: %w", err)
	}
	if url := extractUploadURL(parsed, 0); url != "" {
		return url, nil
	}
	return "", fmt.Errorf("qoder image upload: response missing url")
}

// extractUploadURL 从上传响应里找 OSS URL（上游响应形状不稳定，逐层尝试）。
func extractUploadURL(v any, depth int) string {
	if depth > 3 || v == nil {
		return ""
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	// 先看数组形状：result.imageUrls[0] / image_urls[0] …
	if result, ok := obj["result"].(map[string]any); ok {
		if u := extractUploadURL(result, depth+1); u != "" {
			return u
		}
	}
	for _, key := range []string{"imageUrls", "image_urls"} {
		if arr, ok := obj[key].([]any); ok && len(arr) > 0 {
			if s, ok := arr[0].(string); ok && s != "" {
				return s
			}
		}
	}
	for _, key := range []string{"imageUrl", "image_url", "url", "ossUrl", "oss_url", "originalUrl", "originUrl", "link", "image"} {
		if s, ok := obj[key].(string); ok && s != "" {
			return s
		}
	}
	// 部分上游把 JSON 塞在 body 字符串里
	if body, ok := obj["body"].(string); ok {
		var inner any
		if json.Unmarshal([]byte(body), &inner) == nil {
			return extractUploadURL(inner, depth+1)
		}
	}
	return ""
}
