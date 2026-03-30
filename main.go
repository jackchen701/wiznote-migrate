package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	md "github.com/JohannesKaufmann/html-to-markdown"
	"github.com/JohannesKaufmann/html-to-markdown/plugin"
)

var httpClient = &http.Client{
	Timeout: 30 * time.Second,
}

const maxResponseBytes = 100 * 1024 * 1024 // 100MB
const pageSize = 100
const requestDelay = 300 * time.Millisecond

type ResultCode struct {
	ReturnCode    int    `json:"returnCode"`
	ReturnMessage string `json:"returnMessage"`
}

type WizUserResult struct {
	ResultCode
	Result *WizUser `json:"result"`
}

type WizUser struct {
	UserGuid    string `json:"userGuid"`
	Email       string `json:"email"`
	Mobile      string `json:"mobile"`
	DisplayName string `json:"displayName"`
	KbType      string `json:"kbType"`
	KbServer    string `json:"kbServer"`
	Token       string `json:"token"`
	KbGuid      string `json:"kbGuid"`
}

type DocListResult struct {
	ResultCode
	Result []*Doc `json:"result"`
}

type Doc struct {
	DocGuid         string `json:"docGuid"`
	Title           string `json:"title"`
	Category        string `json:"category"`
	AttachmentCount int    `json:"attachmentCount"`
	Created         int    `json:"created"`
	Accessed        int    `json:"accessed"`
	Keywords        string `json:"keywords"`
	CoverImage      string `json:"coverImage"`
}

type CategoryListResult struct {
	ResultCode
	Result []string `json:"result"`
}

type DownloadResult struct {
	ResultCode
	Info      *DocInfo   `json:"info"`
	HTML      string     `json:"html"`
	Resources []Resource `json:"resources"`
}

type DocInfo struct {
	KbGuid       string `json:"kbGuid"`
	DocGuid      string `json:"docGuid"`
	Type         string `json:"type"`
	AbstractText string `json:"abstractText"`
}

type Resource struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

var conv = md.NewConverter("", true, nil)

var collaborationWarningRe = regexp.MustCompile(`warning-icon|client version.*too low|当前客户端版本较低`)

func main() {
	outputDir := flag.String("output", ".", "export output directory")
	userIdFlag := flag.String("userId", "", "wiz userId (prefer env WIZ_USER_ID)")
	passwordFlag := flag.String("password", "", "wiz password (prefer env WIZ_PASSWORD)")
	foldersFlag := flag.String("folders", "", "export folders, e.g. /日记/,/Logs/")
	skipExisting := flag.Bool("skip-existing", false, "skip notes whose .md file already exists (useful for resuming)")
	flag.Parse()

	userId := os.Getenv("WIZ_USER_ID")
	if userId == "" {
		userId = *userIdFlag
	}
	password := os.Getenv("WIZ_PASSWORD")
	if password == "" {
		password = *passwordFlag
	}
	folders := *foldersFlag

	if userId == "" || password == "" || folders == "" {
		fmt.Fprintln(os.Stderr, "错误：缺少必要参数。")
		fmt.Fprintln(os.Stderr, "推荐做法：通过环境变量传递凭据，避免密码出现在命令行历史中：")
		fmt.Fprintln(os.Stderr, "  export WIZ_USER_ID='your@email.com'")
		fmt.Fprintln(os.Stderr, "  export WIZ_PASSWORD='yourpassword'")
		fmt.Fprintln(os.Stderr, "  wiz_export --output '/path/to/output' --folders '/日记/,/工作/'")
		fmt.Fprintln(os.Stderr, "")
		flag.PrintDefaults()
		os.Exit(1)
	}

	root := *outputDir

	conv.Use(plugin.GitHubFlavored())

	wizUser, err := Login(userId, password)
	if err != nil {
		fmt.Fprintf(os.Stderr, "登录失败: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("登录成功:\n\tkbServer: %s\n\tkbGuid: %s\n\tuser: %s\n",
		wizUser.KbServer, wizUser.KbGuid, wizUser.DisplayName)

	if err := validateKbServer(wizUser.KbServer); err != nil {
		fmt.Fprintf(os.Stderr, "KbServer 校验失败: %v\n", err)
		os.Exit(1)
	}

	hasError := false
	folderArr := strings.Split(folders, ",")
	for _, folder := range folderArr {
		folder = strings.TrimSpace(folder)
		if folder == "" {
			continue
		}
		if !strings.HasSuffix(folder, "/") {
			folder = folder + "/"
		}
		fmt.Printf("正在导出文件夹（含子目录）: %s\n", folder)
		if err := fetchFolderRecursive(root, wizUser, folder, *skipExisting); err != nil {
			fmt.Fprintf(os.Stderr, "导出失败 [%s]: %v\n", folder, err)
			hasError = true
		}
	}

	if hasError {
		fmt.Fprintln(os.Stderr, "\n部分内容导出失败，请查看上方错误信息。")
		os.Exit(1)
	}
	fmt.Println("\n导出完成。")
}

func fetchFolderRecursive(root string, wizUser *WizUser, folder string, skipExisting bool) error {
	if err := fetchFolder(root, wizUser, folder, skipExisting); err != nil {
		fmt.Fprintf(os.Stderr, "  导出文件夹失败 [%s]: %v\n", folder, err)
	}

	subFolders, err := fetchDirectSubCategories(wizUser, folder)
	if err != nil {
		return fmt.Errorf("获取子文件夹列表失败 [%s]: %w", folder, err)
	}

	for _, sub := range subFolders {
		fmt.Printf("  进入子目录: %s\n", sub)
		if err := fetchFolderRecursive(root, wizUser, sub, skipExisting); err != nil {
			fmt.Fprintf(os.Stderr, "  子目录导出失败 [%s]: %v\n", sub, err)
		}
		time.Sleep(requestDelay)
	}

	return nil
}

// fetchDirectSubCategories 获取指定文件夹的直接子文件夹。
// 注意：API 返回全库所有子孙路径，需过滤出恰好多一层的直接子目录。
func fetchDirectSubCategories(wizUser *WizUser, folder string) ([]string, error) {
	apiURL := fmt.Sprintf(
		"%s/ks/category/all/%s?start=0&count=200&parent=%s",
		wizUser.KbServer,
		wizUser.KbGuid,
		url.QueryEscape(folder),
	)

	data, err := Fetch(apiURL, wizUser.Token)
	if err != nil {
		return nil, err
	}

	result := new(CategoryListResult)
	if err = json.Unmarshal(data, result); err != nil {
		return nil, fmt.Errorf("解析子文件夹列表失败: %w", err)
	}
	if result.ReturnCode != 200 {
		return nil, fmt.Errorf("API 错误 code=%d: %s", result.ReturnCode, result.ReturnMessage)
	}

	var direct []string
	for _, sub := range result.Result {
		if isDirectChild(folder, sub) {
			direct = append(direct, sub)
		}
	}
	return direct, nil
}

// isDirectChild 判断 sub 是否为 parent 的直接子目录（不含孙级）。
func isDirectChild(parent, sub string) bool {
	if !strings.HasPrefix(sub, parent) {
		return false
	}
	rest := strings.TrimSuffix(sub[len(parent):], "/")
	return rest != "" && !strings.Contains(rest, "/")
}

func fetchFolder(root string, wizUser *WizUser, folder string, skipExisting bool) error {
	if len(folder) == 0 {
		return fmt.Errorf("folder 为空")
	}

	folderSub := strings.TrimPrefix(folder, "/")
	parentPath, err := safeJoinPath(root, folderSub)
	if err != nil {
		return fmt.Errorf("文件夹路径不安全: %w", err)
	}
	if err = os.MkdirAll(parentPath, 0750); err != nil {
		return fmt.Errorf("创建目录失败 %s: %w", parentPath, err)
	}

	totalFetched := 0
	start := 0
	for {
		docs, err := fetchDocPage(wizUser, folder, start, pageSize)
		if err != nil {
			return fmt.Errorf("拉取笔记列表失败 (start=%d): %w", start, err)
		}

		if len(docs) == 0 {
			break
		}

		fmt.Printf("  [%s] 第 %d-%d 篇\n", folder, start+1, start+len(docs))

		for _, doc := range docs {
			fmt.Printf("    导出文档: %s\n", doc.Title)
			if err := fetchDoc(parentPath, wizUser, doc, skipExisting); err != nil {
				fmt.Fprintf(os.Stderr, "    fetchDoc 失败 [%s]: %v\n", doc.Title, err)
			}
			time.Sleep(requestDelay)
		}

		totalFetched += len(docs)
		if len(docs) < pageSize {
			break
		}
		start += pageSize
	}

	if totalFetched > 0 {
		fmt.Printf("  [%s] 共导出 %d 篇笔记\n", folder, totalFetched)
	}
	return nil
}

func fetchDocPage(wizUser *WizUser, folder string, start, count int) ([]*Doc, error) {
	apiURL := fmt.Sprintf(
		"%s/ks/note/list/category/%s?start=%d&count=%d&category=%s&orderBy=created",
		wizUser.KbServer,
		wizUser.KbGuid,
		start,
		count,
		url.PathEscape(folder),
	)

	data, err := Fetch(apiURL, wizUser.Token)
	if err != nil {
		return nil, err
	}

	result := new(DocListResult)
	if err = json.Unmarshal(data, result); err != nil {
		return nil, fmt.Errorf("解析笔记列表失败: %w", err)
	}
	if result.ReturnCode != 200 {
		return nil, fmt.Errorf("API 错误 code=%d: %s", result.ReturnCode, result.ReturnMessage)
	}

	return result.Result, nil
}

func fetchDoc(parentPath string, wizUser *WizUser, doc *Doc, skipExisting bool) error {
	docBaseName := safeFileName(doc.Title)
	if !strings.HasSuffix(docBaseName, ".md") {
		docBaseName = docBaseName + ".md"
	}

	docPath, err := safeJoinPath(parentPath, docBaseName)
	if err != nil {
		return fmt.Errorf("文档路径不安全 [%s]: %w", doc.Title, err)
	}

	if skipExisting {
		if _, statErr := os.Stat(docPath); statErr == nil {
			fmt.Printf("      [跳过] 已存在: %s\n", docBaseName)
			return nil
		}
	}

	dlResult, err := fetchDownload(wizUser, doc.DocGuid)
	if err != nil {
		return fmt.Errorf("下载笔记失败: %w", err)
	}

	htmlContent := dlResult.HTML
	noteType := ""
	abstractText := ""
	if dlResult.Info != nil {
		noteType = dlResult.Info.Type
		abstractText = dlResult.Info.AbstractText
	}

	var markdown string

	// 协作笔记（note-plus 格式）：REST API 无法读取完整内容，降级写入摘要和原始链接。
	if noteType == "collaboration" || collaborationWarningRe.MatchString(htmlContent) {
		noteURL := fmt.Sprintf(
			"https://as.wiz.cn/note-plus/note/%s/%s",
			wizUser.KbGuid, doc.DocGuid,
		)
		var sb strings.Builder
		sb.WriteString("> **[协作笔记]** 此笔记使用 note-plus 格式存储，REST API 无法导出完整内容。\n")
		sb.WriteString(fmt.Sprintf("> 原始笔记：%s\n", noteURL))
		if abstractText != "" {
			sb.WriteString("\n---\n\n")
			sb.WriteString("**摘要片段（截断）：**\n\n")
			sb.WriteString(abstractText)
			sb.WriteString("\n")
		}
		markdown = sb.String()
		fmt.Printf("      [协作笔记] 无法导出完整内容，仅写入摘要链接: %s\n", doc.Title)
	} else {
		resMap := buildResourceMap(dlResult.Resources)

		noteBaseName := strings.TrimSuffix(docBaseName, ".md")
		assetsRelDir := filepath.Join("assets", noteBaseName)
		assetsAbsDir, err := safeJoinPath(parentPath, assetsRelDir)
		if err != nil {
			return fmt.Errorf("assets 路径不安全: %w", err)
		}

		processedHTML := rewriteIndexFiles(htmlContent, assetsRelDir)

		markdown, err = conv.ConvertString(processedHTML)
		if err != nil {
			return fmt.Errorf("HTML 转 Markdown 失败: %w", err)
		}

		imgNames := extractIndexFileNames(htmlContent)
		if len(imgNames) > 0 {
			if err := os.MkdirAll(assetsAbsDir, 0750); err != nil {
				return fmt.Errorf("创建 assets 目录失败: %w", err)
			}
			for _, fname := range imgNames {
				fmt.Printf("      下载图片: %s\n", fname)
				if err := fetchResource(assetsAbsDir, wizUser, doc, fname, resMap); err != nil {
					fmt.Fprintf(os.Stderr, "      下载图片失败 [%s]: %v\n", fname, err)
				}
				time.Sleep(requestDelay)
			}
		}
	}

	if err := os.WriteFile(docPath, []byte(markdown), 0640); err != nil {
		return fmt.Errorf("写入文件失败 %s: %w", docPath, err)
	}

	return nil
}

func fetchDownload(wizUser *WizUser, docGuid string) (*DownloadResult, error) {
	apiURL := fmt.Sprintf(
		"%s/ks/note/download/%s/%s?downloadData=1",
		wizUser.KbServer, wizUser.KbGuid, docGuid,
	)

	data, err := Fetch(apiURL, wizUser.Token)
	if err != nil {
		return nil, err
	}

	result := new(DownloadResult)
	if err = json.Unmarshal(data, result); err != nil {
		return nil, fmt.Errorf("解析 download 响应失败: %w", err)
	}
	if result.ReturnCode != 200 {
		return nil, fmt.Errorf("download API 错误 code=%d: %s", result.ReturnCode, result.ReturnMessage)
	}

	return result, nil
}

func buildResourceMap(resources []Resource) map[string]string {
	m := make(map[string]string, len(resources))
	for _, r := range resources {
		if r.Name != "" && r.URL != "" {
			m[r.Name] = r.URL
		}
	}
	return m
}

func extractIndexFileNames(html string) []string {
	re := regexp.MustCompile(`index_files/([^\s"'<>]+)`)
	matches := re.FindAllStringSubmatch(html, -1)
	seen := make(map[string]bool)
	var names []string
	for _, m := range matches {
		if !seen[m[1]] {
			seen[m[1]] = true
			names = append(names, m[1])
		}
	}
	return names
}

func rewriteIndexFiles(html, assetsRelDir string) string {
	re := regexp.MustCompile(`index_files/([^\s"'<>]+)`)
	return re.ReplaceAllStringFunc(html, func(match string) string {
		fname := match[len("index_files/"):]
		return filepath.ToSlash(filepath.Join(assetsRelDir, fname))
	})
}

func fetchResource(destDir string, wizUser *WizUser, doc *Doc, fileName string, resMap map[string]string) error {
	safeFile := filepath.Base(strings.ReplaceAll(fileName, "\\", "/"))
	if safeFile == "." || safeFile == "/" || safeFile == "" {
		return fmt.Errorf("资源文件名无效: %q", fileName)
	}

	destPath, err := safeJoinPath(destDir, safeFile)
	if err != nil {
		return fmt.Errorf("资源路径不安全 [%s]: %w", fileName, err)
	}

	if _, statErr := os.Stat(destPath); statErr == nil {
		return nil
	}

	var data []byte

	if signedURL, ok := resMap[safeFile]; ok {
		data, err = FetchURL(signedURL, wizUser.Token)
	} else {
		fallbackURL := fmt.Sprintf(
			"%s/ks/note/view/%s/%s/index_files/%s",
			wizUser.KbServer, wizUser.KbGuid, doc.DocGuid, url.PathEscape(fileName),
		)
		data, err = FetchURL(fallbackURL, wizUser.Token)
	}
	if err != nil {
		return fmt.Errorf("下载资源失败: %w", err)
	}

	if err := os.WriteFile(destPath, data, 0640); err != nil {
		return fmt.Errorf("写入资源文件失败 %s: %w", destPath, err)
	}
	return nil
}

func safeJoinPath(root, sub string) (string, error) {
	joined := filepath.Join(root, sub)
	cleaned := filepath.Clean(joined)
	rootClean := filepath.Clean(root)

	if cleaned != rootClean && !strings.HasPrefix(cleaned, rootClean+string(os.PathSeparator)) {
		return "", fmt.Errorf("路径遍历检测：%q 超出根目录 %q", sub, root)
	}
	return cleaned, nil
}

func safeFileName(name string) string {
	base := filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	if base == "." || base == "/" {
		return "_unnamed"
	}
	re := regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)
	safe := re.ReplaceAllString(base, "_")
	if safe == "" {
		return "_unnamed"
	}
	return safe
}

func Login(userId, password string) (*WizUser, error) {
	body := map[string]string{"userId": userId, "password": password}
	bs, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("序列化登录请求失败: %w", err)
	}

	resp, err := httpClient.Post("https://as.wiz.cn/as/user/login", "application/json", bytes.NewReader(bs))
	if err != nil {
		return nil, fmt.Errorf("登录请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("登录 HTTP 错误: %s", resp.Status)
	}

	rs, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("读取登录响应失败: %w", err)
	}

	ur := new(WizUserResult)
	if err = json.Unmarshal(rs, ur); err != nil {
		return nil, fmt.Errorf("解析登录响应失败: %w", err)
	}
	if ur.ReturnCode != 200 {
		return nil, fmt.Errorf("登录失败: %s", ur.ReturnMessage)
	}

	return ur.Result, nil
}

// validateKbServer 校验 KbServer 必须是 wiz.cn 域名且使用 HTTPS。
func validateKbServer(kbServer string) error {
	u, err := url.Parse(kbServer)
	if err != nil {
		return fmt.Errorf("无法解析 KbServer URL: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("KbServer 必须使用 HTTPS，当前: %s", u.Scheme)
	}
	host := u.Hostname()
	if !strings.HasSuffix(host, ".wiz.cn") && host != "wiz.cn" {
		return fmt.Errorf("KbServer 主机名不在白名单内: %s（只允许 *.wiz.cn）", host)
	}
	return nil
}

// Fetch 发起带 Token 认证的 GET 请求并打印日志。
func Fetch(reqURL, token string) ([]byte, error) {
	fmt.Println("\tfetch:", reqURL)
	return FetchURL(reqURL, token)
}

// FetchURL 发起带 Token 认证的 GET 请求（不打印日志，供图片下载使用）。
func FetchURL(reqURL, token string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("X-Wiz-Token", token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP 请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP 错误: %s", resp.Status)
	}

	rs, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	return rs, nil
}
