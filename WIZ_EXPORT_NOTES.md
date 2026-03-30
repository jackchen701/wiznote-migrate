# 为知笔记导出工具开发经验记录

## 一、为知笔记 REST API 概览

### 登录

```
POST https://as.wiz.cn/as/user/login
Body: {"userId": "xxx@qq.com", "password": "xxx"}
返回: { result: { token, kbServer, kbGuid, ... } }
```

- `token`：后续所有请求在 Header 里带 `X-Wiz-Token: <token>`
- `kbServer`：用户所在的知识库服务器，如 `https://vipkshttps6.wiz.cn`
- `kbGuid`：用户知识库 GUID

**频控注意**：登录接口有严格频控（`WizErrorRateLimit`），token 有效期较长，务必缓存复用，不要频繁重新登录。

---

### 获取文件夹列表

```
GET {kbServer}/ks/category/all/{kbGuid}?start=0&count=200&parent={folder}
```

**关键 Bug 陷阱**：

该接口返回的是**以 `parent` 为根的所有子孙文件夹的全局绝对路径列表**，而不是 `parent` 的直接子目录。

例如，`parent=/学习提升/内容推广/` 时，返回的是整个知识库所有文件夹：
```json
["/Lite/", "/My Notes/", "/人生/", "/人生/健康/", ...]
```

这会导致**无限递归死循环**：每次拿到的列表都一样，永远不会停止。

**正确做法**：过滤出比 `parent` 恰好多一层路径分隔符的条目：

```go
func isDirectChild(parent, sub string) bool {
    if !strings.HasPrefix(sub, parent) {
        return false
    }
    rest := strings.TrimSuffix(sub[len(parent):], "/")
    return rest != "" && !strings.Contains(rest, "/")
}
```

---

### 获取笔记列表

```
GET {kbServer}/ks/note/list/category/{kbGuid}?start=0&count=100&category={folder}&orderBy=created
```

返回 `result` 数组，每条包含 `docGuid`、`title`、`category`、`attachmentCount` 等。

---

### 获取笔记内容

**不要用**：
```
GET {kbServer}/ks/note/view/{kbGuid}/{docGuid}?objType=document
```
对协作笔记（note-plus 格式）会返回升级提示 HTML 页，不是真实内容。

**正确做法**，用 download API：
```
GET {kbServer}/ks/note/download/{kbGuid}/{docGuid}?downloadData=1
```

返回结构：
```json
{
  "returnCode": 200,
  "info": {
    "type": "collaboration",   // 协作笔记标志
    "abstractText": "...",     // 摘要文本（截断，非完整内容）
    "dataSize": 100,           // 不可靠，不能用于判断内容大小
    ...
  },
  "html": "...",              // 普通笔记的完整 HTML；协作笔记是升级警告页
  "resources": [              // 附件/图片列表（含带签名的下载 URL）
    { "name": "xxx.jpg", "url": "https://...?wiz_signature=..." }
  ]
}
```

---

## 二、协作笔记（collaboration type）问题

### 问题描述

为知笔记有两套笔记系统：

| 类型 | 标志 | 存储方式 | API 可访问 |
|------|------|----------|-----------|
| 普通笔记 | `type: ""` | HTML 文件 + index_files/ 附件 | ✅ 完整内容 |
| 协作笔记 | `type: "collaboration"` | note-plus 后端（CRDT/OT 协同编辑） | ❌ 仅摘要 |

### 根本原因

协作笔记（note-plus 格式）的内容存储在 `note-plus.wiz.cn` 专属后端，采用 CRDT/OT 协议进行实时协同编辑。REST API 没有开放这部分内容的读取接口。

- `download?downloadData=1` 返回的 `html` 字段是一个升级提示页（约 5198 字节固定大小），不是笔记内容
- `note-plus.wiz.cn` 域名在国内网络环境部分不可达（SSL EOF）
- `abstractText` 字段仅有约 100-200 字符的截断摘要

### 识别方式

```go
// 方式一：检查 info.type 字段（最可靠）
if info.Type == "collaboration" { ... }

// 方式二：检查 html 内容特征（兜底）
var collaborationWarningRe = regexp.MustCompile(`note-plus-style|warning-icon`)
if collaborationWarningRe.MatchString(html) { ... }
```

### 当前限制

通过公开 REST API **无法获取协作笔记的完整内容**，只能降级处理：
- 写入摘要（`abstractText`）并注明无法导出的原因
- 保留原始笔记链接，方便手动查看

---

## 三、图片/附件下载

### 旧格式笔记（普通笔记）

HTML 中图片引用格式：
```html
<img src="index_files/xxxxxx.jpg">
```

下载方式：优先使用 `resources[]` 里的带签名 URL。

```json
"resources": [
  {
    "name": "xxxxxx.jpg",
    "url": "https://{kbServer}/ks/object/download/{kbGuid}/{docGuid}?objType=resource&objId=xxx.jpg&wiz_signature=xxx"
  }
]
```

**不要**直接拼接 `/ks/note/view/{kbGuid}/{docGuid}/index_files/{filename}`，这个路径没有签名，可能鉴权失败。

降级路径（`resources[]` 中找不到时）：
```
GET {kbServer}/ks/note/view/{kbGuid}/{docGuid}/index_files/{filename}
```

### Obsidian 风格目录结构

将所有 `index_files/xxx.jpg` 替换为 `assets/<笔记名>/xxx.jpg`，与笔记文件并列存放：

```
内容推广/
  小说推广.md          ← 引用 assets/小说推广/img.jpg
  assets/
    小说推广/
      img.jpg
```

在 HTML 转 Markdown 之前，先做字符串替换：
```go
re.ReplaceAll("index_files/xxx.jpg" → "assets/笔记名/xxx.jpg")
```

---

## 四、频控策略

- 每次 API 请求后至少等待 300ms
- 登录 token 缓存复用（不要每次导出都重新登录）
- 遇到 `WizErrorRateLimit` 后等待 60s 以上再重试

---
