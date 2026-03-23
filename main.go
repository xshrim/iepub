package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/PuerkitoBio/goquery"
	"github.com/alecthomas/chroma/v2"
	chtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"

	"github.com/go-shiori/go-epub"
	"github.com/saintfish/chardet"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
	"golang.org/x/text/transform"
)

type AdvancedConfig struct {
	InputPath   string
	OutputPath  string
	Title       string
	Creator     string
	Contributor string
	Language    string
	Description string
	Subject     string
	Date        string
	Publisher   string
	Rights      string
	CoverPath   string
	CssPath     string
	illusPath   string
	ChapterRe   string
	ImgRe       string // 图片嵌入正则，例如 [IMG:1.jpg]
	Llm         string // 大模型配置，例如 glm/glm-4-flash:xxxx
	Wait        int    // 大模型调用毫秒间隔
	Proxy       string
	Rate        string
	Info        bool
	Htime       bool
	Port        int
}

type OPFPackage struct {
	XMLName  xml.Name `xml:"package"`
	Metadata struct {
		Title       string   `xml:"http://purl.org/dc/elements/1.1/ title"`
		Creator     []string `xml:"http://purl.org/dc/elements/1.1/ creator"`
		Contributor []string `xml:"http://purl.org/dc/elements/1.1/ contributor"`
		Language    string   `xml:"http://purl.org/dc/elements/1.1/ language"`
		Subject     []string `xml:"http://purl.org/dc/elements/1.1/ subject"`
		Description string   `xml:"http://purl.org/dc/elements/1.1/ description"`
		Date        string   `xml:"http://purl.org/dc/elements/1.1/ date"`
		Publisher   string   `xml:"http://purl.org/dc/elements/1.1/ publisher"`
		Rights      string   `xml:"http://purl.org/dc/elements/1.1/ rights"`
	} `xml:"metadata"`
	Manifest struct {
		Items []struct {
			ID         string `xml:"id,attr"`
			Href       string `xml:"href,attr"`
			Properties string `xml:"properties,attr"`
		} `xml:"item"`
	} `xml:"manifest"`
}

type NCX struct {
	XMLName   xml.Name   `xml:"http://www.daisy.org/z3986/2005/ncx/ ncx"`
	Title     string     `xml:"docTitle>text"`
	NavPoints []NavPoint `xml:"navMap>navPoint"`
}

type NavPoint struct {
	Text      string     `xml:"navLabel>text"`
	Content   string     `xml:"content-src,attr"` // 获取 content 标签的 src 属性
	NavPoints []NavPoint `xml:"navPoint"`         // 递归处理嵌套子目录
}

type Spider struct {
	proxy      string
	httpClient *http.Client
	ticker     *time.Ticker
	retries    int
}

// NewSpider 初始化爬虫
func NewSpider(rate, prxy string, retry int) *Spider {
	spider := &Spider{
		proxy: prxy,
		httpClient: &http.Client{
			Timeout: time.Second * 15,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					// 关键：不要使用默认的加密套件顺序
					CipherSuites: []uint16{
						tls.TLS_AES_128_GCM_SHA256,
						tls.TLS_CHACHA20_POLY1305_SHA256,
						tls.TLS_AES_256_GCM_SHA384,
						tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
					},
					CurvePreferences: []tls.CurveID{tls.CurveP256, tls.X25519},
					// 降低被检测概率：模拟真实的随机 SessionTicket
					SessionTicketsDisabled: false,
				},
				// 限制连接池，防止被发现有成百上千个并发连接
				MaxIdleConns:      10,
				IdleConnTimeout:   30 * time.Second,
				DisableKeepAlives: false, // 尽量复用连接以模拟正常浏览
			},
		},
		// 初始化频率限制器
		ticker:  time.NewTicker(time.Millisecond * 100),
		retries: 3,
	}

	if retry > 0 {
		spider.retries = retry
	}

	if rateLimit, err := time.ParseDuration(rate); err == nil {
		spider.ticker = time.NewTicker(rateLimit)
	}

	spider.setProxy()

	return spider
}

// Fetch 执行抓取任务
func (s *Spider) Fetch(targetURL string) (string, error) {
	var lastErr error

	for i := 0; i < s.retries; i++ {
		// 1. 频率控制 + 随机抖动 (Jitter)
		// 固定的频率很容易被防火墙识别，加入随机延迟模拟人工行为
		<-s.ticker.C
		jitter := time.Duration(mrand.IntN(500)) * time.Millisecond
		time.Sleep(jitter)

		// 2. 使用 Context 设置单次请求超时
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		req, err := http.NewRequestWithContext(ctx, "GET", targetURL, nil)
		if err != nil {
			cancel()
			return "", err
		}

		// 3. 完善请求头 (反反爬核心)
		s.setHeaders(req)
		s.setProxy()

		// 4. 显式关闭连接 (防止大量 TIME_WAIT 导致 EOF)
		// 如果对方服务器不稳定，开启此项能显著减少 EOF 错误
		req.Close = true

		resp, err := s.httpClient.Do(req)
		if err != nil {
			cancel()
			lastErr = err
			continue
		}

		// 5. 检查状态码
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			cancel()
			lastErr = fmt.Errorf("status code: %d", resp.StatusCode)

			// 如果被封 IP (403) 或频率过快 (429)，重试前多等会儿
			if resp.StatusCode == 403 || resp.StatusCode == 429 {
				waitTime := 2 * time.Duration(1<<i)
				time.Sleep(waitTime)
				continue
			}
			continue
		}

		// 6. 读取数据
		bodyBytes, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel() // 及时释放 Context 资源

		if err != nil {
			lastErr = err
			continue
		}
		if len(bodyBytes) == 0 {
			lastErr = fmt.Errorf("empty response body")
			continue
		}

		return string(bodyBytes), nil
	}

	return "", lastErr
}

func (s *Spider) setProxy() {
	if s.proxy == "" {
		s.httpClient.Transport.(*http.Transport).Proxy = http.ProxyFromEnvironment
	} else {
		if proxyURL, err := url.Parse(strings.TrimSpace(strings.Split(s.proxy, ",")[mrand.IntN(len(strings.Split(s.proxy, ",")))])); err == nil {
			s.httpClient.Transport.(*http.Transport).Proxy = http.ProxyURL(proxyURL)
		}
	}
}

// 完善 Header 伪装
func (s *Spider) setHeaders(req *http.Request) {
	// 随机选择 User-Agent
	uas := []string{
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/119.0.0.0 Safari/537.36",
		"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
	}
	req.Header.Set("User-Agent", uas[mrand.IntN(len(uas))])

	// 补充必要的浏览器特征 Header
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("sec-ch-ua", `"Not_A Brand";v="8", "Chromium";v="120", "Google Chrome";v="120"`)

	// 关键：模拟来源地址，有些网站会检查 Referer
	referers := []string{
		"https://www.google.com/",
		"https://www.bing.com/",
		"https://duckduckgo.com/",
	}
	req.Header.Set("Referer", referers[mrand.IntN(len(referers))])
}

func main() {
	cfg := AdvancedConfig{}
	flag.BoolVar(&cfg.Info, "i", false, "获取或修改epub元数据")
	flag.StringVar(&cfg.OutputPath, "o", "", "输出文件(默认: 输入文件名.epub)")
	flag.StringVar(&cfg.Title, "t", "", "书名(默认: 输入文件名)")
	flag.StringVar(&cfg.Creator, "a", "", "作者")
	flag.StringVar(&cfg.Contributor, "j", "", "制作者与协作者")
	flag.StringVar(&cfg.Language, "g", "", "语言")
	flag.StringVar(&cfg.Description, "e", "", "描述")
	flag.StringVar(&cfg.Subject, "k", "", "标签")
	flag.StringVar(&cfg.Date, "d", "", "发行日期")
	flag.StringVar(&cfg.Publisher, "u", "", "出版商")
	flag.StringVar(&cfg.Rights, "b", "", "版权声明")
	flag.StringVar(&cfg.CoverPath, "c", "", "封面图片路径")
	flag.StringVar(&cfg.CssPath, "s", "", "样式文件路径")
	flag.StringVar(&cfg.illusPath, "z", "", "插画图片路径")
	flag.StringVar(&cfg.ChapterRe, "r", ``, "章节识别正则(默认: 内置自动检测规则)")
	flag.StringVar(&cfg.ImgRe, "m", `\[IMG:(.*?)\]`, "图片标签识别正则(默认: [IMG:xxx])")
	flag.BoolVar(&cfg.Htime, "v", false, "高亮时间")
	flag.StringVar(&cfg.Llm, "l", "", "大模型补全章节标题(格式: glm/glm-4-flash:xxxx)")
	flag.IntVar(&cfg.Wait, "w", 1000, "大模型调用毫秒间隔(默认: 1000ms)")
	flag.StringVar(&cfg.Proxy, "x", "", "网页请求代理(格式: http://127.0.0.1:1080)")
	flag.StringVar(&cfg.Rate, "n", "1s", "网页请求间隔(默认: 1s)")
	flag.IntVar(&cfg.Port, "p", 2233, "服务端口(默认: 2233)")
	flag.Parse()

	if flag.NArg() > 0 {
		if flag.Arg(0) == "server" {
			server(cfg.Port)
		} else {
			cfg.InputPath = flag.Arg(0)
		}
	}
	if cfg.InputPath == "" {
		fmt.Println("✘ 错误: 请提供输入文件或请求网页")
		fmt.Println("用法示例: iepub input.txt 或 iepub input.epub 或iepub xxx.com/xxx")
		return
	}

	switch strings.ToLower(filepath.Ext(filepath.Base(cfg.InputPath))) {
	case ".txt":
		if _, err := os.Stat(cfg.InputPath); err != nil {
			fmt.Println("✘ 错误: 获取输入文件失败:", err.Error())
			return
		}
		if cfg.OutputPath == "" {
			cfg.OutputPath = strings.TrimSuffix(filepath.Base(cfg.InputPath), filepath.Ext(filepath.Base(cfg.InputPath))) + ".epub"
			fmt.Printf("ℹ 未指定输出文件名，自动使用输入文件名: [%s]\n", cfg.OutputPath)
		}
		if cfg.Title == "" {
			cfg.Title = strings.TrimSuffix(filepath.Base(cfg.InputPath), filepath.Ext(filepath.Base(cfg.InputPath)))
			fmt.Printf("ℹ 未指定书名，自动使用文件名: [%s]\n", cfg.Title)
		}

		meta := make(map[string]string)
		if cfg.Title != "" {
			meta["title"] = cfg.Title
		}
		if cfg.Creator != "" {
			meta["creator"] = cfg.Creator
		}
		if cfg.Contributor != "" {
			meta["contributor"] = cfg.Contributor
		}
		if cfg.Language != "" {
			meta["language"] = cfg.Language
		}
		if cfg.Description != "" {
			meta["description"] = cfg.Description
		}
		if cfg.Subject != "" {
			meta["subject"] = cfg.Subject
		}
		if cfg.Date != "" {
			meta["date"] = cfg.Date
		}
		if cfg.Publisher != "" {
			meta["publisher"] = cfg.Publisher
		}
		if cfg.Rights != "" {
			meta["rights"] = cfg.Rights
		}

		var illusPath []string
		if cfg.illusPath != "" {
			if info, err := os.Stat(cfg.illusPath); err == nil {
				if info.IsDir() {
					files, _ := os.ReadDir(cfg.illusPath)
					for _, f := range files {
						illusPath = append(illusPath, filepath.Join(cfg.illusPath, f.Name()))
					}
					sort.Strings(illusPath)
				} else {
					illusPath = append(illusPath, cfg.illusPath)
				}
			}
		}

		txtToEpub(cfg.InputPath, cfg.OutputPath, cfg.ChapterRe, cfg.CoverPath, cfg.CssPath, illusPath, cfg.Llm, cfg.Wait, cfg.Htime, meta, cfg.Proxy)
	case ".epub":
		if _, err := os.Stat(cfg.InputPath); err != nil {
			fmt.Println("✘ 错误: 获取输入文件失败:", err.Error())
			return
		}
		if cfg.Info {
			meta := make(map[string]string)
			if cfg.Title != "" {
				meta["title"] = cfg.Title
			}
			if cfg.Creator != "" {
				meta["creator"] = cfg.Creator
			}
			if cfg.Contributor != "" {
				meta["contributor"] = cfg.Contributor
			}
			if cfg.Language != "" {
				meta["language"] = cfg.Language
			}
			if cfg.Description != "" {
				meta["description"] = cfg.Description
			}
			if cfg.Subject != "" {
				meta["subject"] = cfg.Subject
			}
			if cfg.Date != "" {
				meta["date"] = cfg.Date
			}
			if cfg.Publisher != "" {
				meta["publisher"] = cfg.Publisher
			}
			if cfg.Rights != "" {
				meta["rights"] = cfg.Rights
			}

			if len(meta) > 0 || cfg.CoverPath != "" || cfg.illusPath != "" {
				var illusPath []string
				if cfg.illusPath != "" {
					if info, err := os.Stat(cfg.illusPath); err == nil {
						if info.IsDir() {
							files, _ := os.ReadDir(cfg.illusPath)
							for _, f := range files {
								illusPath = append(illusPath, filepath.Join(cfg.illusPath, f.Name()))
							}
							sort.Strings(illusPath)
						} else {
							illusPath = append(illusPath, cfg.illusPath)
						}
					}
				}
				metaEdit(cfg.InputPath, cfg.OutputPath, cfg.CoverPath, illusPath, meta, cfg.Proxy)
			} else {
				if meta, err := metaView(cfg.InputPath); err != nil {
					fmt.Printf("✘ 未找到元数据: %v\n", err)
				} else {
					fmt.Printf("书名(Title): %s\n", meta["title"])
					fmt.Printf("作者(Creator): %s\n", meta["creator"])
					fmt.Printf("协作(Contributor): %s\n", meta["contributor"])
					fmt.Printf("语言(Language): %s\n", meta["language"])
					fmt.Printf("描述(Description): %s\n", meta["description"])
					fmt.Printf("标签(Subject): %s\n", meta["subject"])
					fmt.Printf("发行(Date): %s\n", meta["date"])
					fmt.Printf("版商(Publisher): %s\n", meta["publisher"])
					fmt.Printf("版权(Rights): %s\n", meta["rights"])
					fmt.Printf("封面(Cover): %s\n", meta["cover"])
					fmt.Printf("插画(Illustration): %s\n", meta["illustration"])
					fmt.Printf("目录(Toc):\n")
					for _, chapter := range strings.Split(meta["toc"], "|||") {
						chp := strings.Split(chapter, "@@@")
						chpname := chp[0]
						chpitem := chp[1]
						fmt.Printf("  - %s\n", chpname)
						if chpitem != "" {
							for _, item := range strings.Split(chpitem, ":::") {
								fmt.Printf("    - %s\n", item)
							}
						}
					}
				}
			}

		} else {
			if cfg.OutputPath == "" {
				cfg.OutputPath = strings.TrimSuffix(filepath.Base(cfg.InputPath), filepath.Ext(filepath.Base(cfg.InputPath))) + ".txt"
				fmt.Printf("ℹ 未指定输出文件名，自动使用输入文件名: [%s]\n", cfg.OutputPath)
			}
			epubToTxt(cfg.InputPath, cfg.OutputPath)
		}
	default: // 爬取网页
		crawl(cfg.InputPath, cfg.Rate, cfg.Proxy, 3)
	}
}

func server(port int) {
	http.HandleFunc("/crawl", handleCrawl)
	http.HandleFunc("/convert", handleConvert)
	http.HandleFunc("/metadata", handleGetMetadata)
	http.HandleFunc("/edit", handleEditMetadata)

	fmt.Printf("▶ 启动服务模式: http://0.0.0.0:%d\n", port)
	http.ListenAndServe(fmt.Sprintf(":%d", port), nil)
}

func handleCrawl(w http.ResponseWriter, r *http.Request) {
	upath := r.FormValue("url")
	rate := r.FormValue("rate")
	proxy := r.FormValue("proxy")

	if upath == "" {
		http.Error(w, "请输入请求URL", http.StatusBadRequest)
	}

	opath, err := crawl(upath, rate, proxy, 3)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}

	w.Header().Set("Content-Disposition", "attachment; filename="+filepath.Base(opath))
	http.ServeFile(w, r, opath)
	os.Remove(opath)
}

func handleGetMetadata(w http.ResponseWriter, r *http.Request) {
	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "请上传文件", http.StatusBadRequest)
		return
	}
	defer file.Close()

	tempFile, _ := os.CreateTemp("", "*.epub")
	defer os.Remove(tempFile.Name())
	io.Copy(tempFile, file)

	meta, err := metaView(tempFile.Name())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	fmt.Fprint(w, meta)
}

func handleEditMetadata(w http.ResponseWriter, r *http.Request) {
	r.ParseMultipartForm(32 << 20) // 32MB max

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "请上传文件", http.StatusBadRequest)
		return
	}
	defer file.Close()

	tempInput := filepath.Join(os.TempDir(), "input_"+header.Filename)
	tempOutput := filepath.Join(os.TempDir(), "output_"+header.Filename)
	f, _ := os.Create(tempInput)
	io.Copy(f, file)
	f.Close()
	defer os.Remove(tempInput)

	proxy := r.FormValue("proxy")

	meta := make(map[string]string)

	if title := r.FormValue("title"); title != "" {
		meta["title"] = title
	}
	if creator := r.FormValue("creator"); creator != "" {
		meta["creator"] = creator
	}
	if contributor := r.FormValue("contributor"); contributor != "" {
		meta["contributor"] = contributor
	}
	if language := r.FormValue("language"); language != "" {
		meta["language"] = language
	}
	if description := r.FormValue("description"); description != "" {
		meta["description"] = description
	}
	if subject := r.FormValue("subject"); subject != "" {
		meta["subject"] = subject
	}
	if date := r.FormValue("date"); date != "" {
		meta["date"] = date
	}
	if publisher := r.FormValue("publisher"); publisher != "" {
		meta["publisher"] = publisher
	}
	if rights := r.FormValue("rights"); rights != "" {
		meta["rights"] = rights
	}

	newCoverPath := ""
	coverFile, coverHeader, err := r.FormFile("cover")
	if err == nil {
		tempCover := filepath.Join(os.TempDir(), coverHeader.Filename)
		cf, _ := os.Create(tempCover)
		io.Copy(cf, coverFile)
		cf.Close()
		newCoverPath = tempCover
		defer os.Remove(tempCover)
	}
	var newillusPath []string
	illustrations := r.MultipartForm.File["illustrations"]
	for _, illusHeader := range illustrations {
		illusFile, _ := illusHeader.Open()
		defer illusFile.Close()
		tempIllus := filepath.Join(os.TempDir(), illusHeader.Filename)
		cf, _ := os.Create(tempIllus)
		io.Copy(cf, illusFile)
		cf.Close()
		newillusPath = append(newillusPath, tempIllus)
		defer os.Remove(tempIllus)
	}

	err = metaEdit(tempInput, tempOutput, newCoverPath, newillusPath, meta, proxy)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Disposition", "attachment; filename=modified_"+header.Filename)
	http.ServeFile(w, r, tempOutput)
	os.Remove(tempOutput)
}

func handleConvert(w http.ResponseWriter, r *http.Request) {
	r.ParseMultipartForm(32 << 20) // 32MB max

	file, header, _ := r.FormFile("file")
	ext := filepath.Ext(header.Filename)

	tempInput := filepath.Join(os.TempDir(), header.Filename)
	f, _ := os.Create(tempInput)
	io.Copy(f, file)
	f.Close()
	defer os.Remove(tempInput)

	var outputName string
	var targetPath string

	proxy := r.FormValue("proxy")

	meta := make(map[string]string)
	// 2. 获取修改参数
	if title := r.FormValue("title"); title != "" {
		meta["title"] = title
	}
	if creator := r.FormValue("creator"); creator != "" {
		meta["creator"] = creator
	}
	if contributor := r.FormValue("contributor"); contributor != "" {
		meta["contributor"] = contributor
	}
	if language := r.FormValue("language"); language != "" {
		meta["language"] = language
	}
	if description := r.FormValue("description"); description != "" {
		meta["description"] = description
	}
	if subject := r.FormValue("subject"); subject != "" {
		meta["subject"] = subject
	}
	if date := r.FormValue("date"); date != "" {
		meta["date"] = date
	}
	if publisher := r.FormValue("publisher"); publisher != "" {
		meta["publisher"] = publisher
	}
	if rights := r.FormValue("rights"); rights != "" {
		meta["rights"] = rights
	}

	chpre := r.FormValue("chpre")
	llm := r.FormValue("llm")
	wait := 300
	if iwait := r.FormValue("wait"); iwait != "" {
		wait, _ = strconv.Atoi(iwait)
	}
	htime := false
	if strings.ToLower(r.FormValue("htime")) == "true" {
		htime = true
	}

	var newCoverPath, newCssPath string
	coverFile, coverHeader, err := r.FormFile("cover")
	if err == nil {
		tempCover := filepath.Join(os.TempDir(), coverHeader.Filename)
		cf, _ := os.Create(tempCover)
		io.Copy(cf, coverFile)
		cf.Close()
		newCoverPath = tempCover
		defer os.Remove(tempCover)
	}
	cssFile, cssHeader, err := r.FormFile("css")
	if err == nil {
		tempCss := filepath.Join(os.TempDir(), cssHeader.Filename)
		cf, _ := os.Create(tempCss)
		io.Copy(cf, cssFile)
		cf.Close()
		newCssPath = tempCss
		defer os.Remove(tempCss)
	}
	var newillusPath []string
	illustrations := r.MultipartForm.File["illustrations"]
	for _, illusHeader := range illustrations {
		illusFile, _ := illusHeader.Open()
		defer illusFile.Close()
		tempIllus := filepath.Join(os.TempDir(), illusHeader.Filename)
		cf, _ := os.Create(tempIllus)
		io.Copy(cf, illusFile)
		cf.Close()
		newillusPath = append(newillusPath, tempIllus)
		defer os.Remove(tempIllus)
	}

	switch ext {
	case ".epub":
		outputName = strings.TrimSuffix(header.Filename, ".epub") + ".txt"
		targetPath = filepath.Join(os.TempDir(), outputName)
		epubToTxt(tempInput, targetPath)
	case ".txt":
		outputName = strings.TrimSuffix(header.Filename, ".txt") + ".epub"
		targetPath = filepath.Join(os.TempDir(), outputName)
		txtToEpub(tempInput, targetPath, chpre, newCoverPath, newCssPath, newillusPath, llm, wait, htime, meta, proxy)
	}

	w.Header().Set("Content-Disposition", "attachment; filename="+outputName)
	http.ServeFile(w, r, targetPath)
	os.Remove(targetPath)
}

func metaView(inputPath string) (map[string]string, error) {
	r, err := zip.OpenReader(inputPath)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	var meta = make(map[string]string)

	for _, f := range r.File {
		if strings.HasSuffix(f.Name, ".opf") {
			var cover string
			rc, _ := f.Open()
			buf, _ := io.ReadAll(rc)
			rc.Close()

			var pkg OPFPackage
			xml.Unmarshal(buf, &pkg)

			for _, item := range pkg.Manifest.Items {
				if strings.Contains(item.Properties, "cover-image") || item.ID == "cover" || item.ID == "cover-image" {
					if regexp.MustCompile(`(?i)\.(jpg|jpeg|png|bmp|gif|webp|avif|heic|heif|svg|svgz|tif|tiff|ico)$`).MatchString(f.Name) {
						cover = path.Join(path.Dir(f.Name), item.Href)
						break
					}
				}
			}
			meta["title"] = pkg.Metadata.Title
			meta["creator"] = strings.Join(pkg.Metadata.Creator, ",")
			meta["contributor"] = strings.Join(pkg.Metadata.Contributor, ",")
			meta["language"] = pkg.Metadata.Language
			meta["description"] = pkg.Metadata.Description
			meta["subject"] = strings.Join(pkg.Metadata.Subject, ",")
			meta["date"] = pkg.Metadata.Date
			meta["publisher"] = pkg.Metadata.Publisher
			meta["rights"] = pkg.Metadata.Rights
			meta["cover"] = cover
		} else if strings.HasSuffix(f.Name, "illustration.xhtml") {
			rc, _ := f.Open()
			buf, _ := io.ReadAll(rc)
			rc.Close()

			var illuss []string
			reImg := regexp.MustCompile(`(?i)src=["']([^"']+)["']`)
			matches := reImg.FindAllStringSubmatch(string(buf), -1)
			for _, match := range matches {
				if len(match) > 1 {
					fileName := path.Join(path.Dir(f.Name), match[1])
					illuss = append(illuss, fileName)
				}
			}
			meta["illustration"] = strings.Join(illuss, ",")
		} else if strings.HasSuffix(f.Name, ".ncx") {
			rc, _ := f.Open()
			buf, _ := io.ReadAll(rc)
			rc.Close()

			var toc []string
			var ncx NCX
			xml.Unmarshal(buf, &ncx)
			for _, p := range ncx.NavPoints {
				var ss []string
				for _, s := range p.NavPoints {
					ss = append(ss, s.Text)
				}
				toc = append(toc, fmt.Sprintf("%s@@@%s", p.Text, strings.Join(ss, ":::")))
			}
			meta["toc"] = strings.Join(toc, "|||")
		} else {
			continue
		}
	}

	return meta, nil
}

func metaEdit(inputPath, outputPath, newCoverPath string, newillusPath []string, meta map[string]string, proxy string) error {
	if len(meta) == 0 && newCoverPath == "" && len(newillusPath) == 0 {
		return nil
	}

	if newCoverPath != "" {
		if !fileExist(newCoverPath) {
			newCoverPath, _ = downloadImage(newCoverPath, proxy)
			if !fileExist(newCoverPath) {
				newCoverPath = ""
			}
		}
	}

	r, err := zip.OpenReader(inputPath)
	if err != nil {
		return err
	}

	var opfPath, opfContent, originalCoverHref, opfDir, illusPagePath, illusPageContent string
	var hasIllusPage bool

	for _, f := range r.File {
		if strings.HasSuffix(f.Name, ".opf") {
			opfPath = f.Name
			opfDir = path.Dir(opfPath)
			rc, _ := f.Open()
			buf, _ := io.ReadAll(rc)
			rc.Close()
			opfContent = string(buf)

			var pkg OPFPackage
			xml.Unmarshal(buf, &pkg)

			for _, item := range pkg.Manifest.Items {
				if strings.Contains(item.Properties, "cover-image") || item.ID == "cover" || item.ID == "cover-image" {
					if regexp.MustCompile(`(?i)\.(jpg|jpeg|png|bmp|gif|webp|avif|heic|heif|svg|svgz|tif|tiff|ico)$`).MatchString(f.Name) {
						originalCoverHref = path.Join(opfDir, item.Href)
						break
					}
				}
			}
		}

		if strings.HasSuffix(f.Name, "illustration.xhtml") {
			illusPagePath = f.Name
			rc, _ := f.Open()
			buf, _ := io.ReadAll(rc)
			rc.Close()
			illusPageContent = string(buf)
			hasIllusPage = true
		}
	}

	var illusManifestItems []string
	var addedIllusNodes string
	if !hasIllusPage {
		illusPagePath = path.Join(opfDir, "Text/illustration.xhtml")
		illusPageContent = `<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE html><html xmlns="http://www.w3.org/1999/xhtml xmlns:epub="http://www.idpf.org/2007/ops"><head><title dir="auto">Illustrations</title></head><body dir="auto"><div class='illustration-container' style='text-align: center;'></div></body></html>`
	}

	for _, imgPath := range newillusPath {
		imgName := filepath.Base(imgPath)
		// imgName := fmt.Sprintf("illus_%d%s", time.Now().UnixNano()+int64(i), filepath.Ext(imgPath))
		imgTargetHref := fmt.Sprintf("images/%s", imgName)

		// 注册到 Manifest
		illusManifestItems = append(illusManifestItems,
			fmt.Sprintf(`<item id="%s" href="%s" media-type="%s"/>`, uuid(), imgTargetHref, getMediaType(imgName)))

		// 生成插画节点 (使用之前提到的适配样式)
		addedIllusNodes += fmt.Sprintf("\n<div style='margin-bottom: 20px; text-align: center; margin: 1em 0; page-break-inside: avoid; page-break-after: auto;'><img src='../%s' style='max-width: 100%%; height: auto; display: block; margin: 0 auto;' /></div>", imgTargetHref)
	}

	// 将新插画插入到 HTML 的 body 结束前
	illusPageContent = strings.Replace(illusPageContent, "</div></body>", addedIllusNodes+"</div></body>", 1)

	var out *os.File
	if outputPath != "" {
		out, err = os.Create(outputPath)
		if err != nil {
			return err
		}
	} else { // overwrite
		out, err = os.CreateTemp("", filepath.Base(inputPath)+".tmp")
		if err != nil {
			return err
		}

		outputPath = out.Name()

		defer func() {
			out.Close()
			os.Remove(outputPath)
		}()
	}

	w := zip.NewWriter(out)

	for _, f := range r.File {
		if f.Name == "mimetype" {
			fh := &zip.FileHeader{Name: "mimetype", Method: zip.Store}
			fw, _ := w.CreateHeader(fh)
			rc, _ := f.Open()
			io.Copy(fw, rc)
			rc.Close()
			break
		}
	}

	coverWritten := false
	targetCoverHref := originalCoverHref

	if newCoverPath != "" && targetCoverHref == "" {
		targetCoverHref = path.Join(opfDir, "images/cover"+filepath.Ext(newCoverPath))
	}

	for _, f := range r.File {
		if f.Name == "mimetype" {
			continue
		}

		rc, _ := f.Open()
		fw, _ := w.Create(f.Name)

		if f.Name == opfPath {
			modified := opfContent
			for k, v := range meta {
				modified = ensureMetadataTag(modified, strings.ToLower(k), v)
			}

			if newCoverPath != "" && originalCoverHref == "" {
				reMetaTag := regexp.MustCompile(`(?i)(<metadata[^>]*>)`)
				metaNode := "\n    " + `<meta name="cover" content="cover"/>`
				modified = reMetaTag.ReplaceAllString(modified, "${1}"+metaNode)

				reManiTag := regexp.MustCompile(`(?i)(<manifest[^>]*>)`)
				itemNode := "\n    " + fmt.Sprintf(`<item id="cover" href="%s" media-type="%s" properties="cover-image"/>`,
					strings.TrimPrefix(strings.TrimPrefix(targetCoverHref, opfDir), "/"), getMediaType(newCoverPath))
				modified = reManiTag.ReplaceAllString(modified, "${1}"+itemNode)
			}

			if len(newillusPath) > 0 {
				reMani := regexp.MustCompile(`(?i)(</manifest>)`)
				modified = reMani.ReplaceAllString(modified, strings.Join(illusManifestItems, "\n    ")+"\n  ${1}")
				if !hasIllusPage {
					relPage := strings.TrimPrefix(illusPagePath, opfDir+"/")
					modified = reMani.ReplaceAllString(modified, fmt.Sprintf("\n    <item id=\"illustration.xhtml\" href=\"%s\" media-type=\"application/xhtml+xml\"/>\n  ${1}", relPage))
					modified = regexp.MustCompile(`(?i)(<spine[^>]*>)`).ReplaceAllString(modified, "${1}\n    <itemref idref=\"illustration.xhtml\"/>")
				}
			}

			fw.Write([]byte(modified))
		} else if newCoverPath != "" && f.Name == targetCoverHref {
			newCover, _ := os.ReadFile(newCoverPath)
			fw.Write(newCover)
			coverWritten = true
		} else if f.Name == illusPagePath {
			fw.Write([]byte(illusPageContent))
			hasIllusPage = true
		} else {
			io.Copy(fw, rc)
		}
		rc.Close()
	}

	if newCoverPath != "" && !coverWritten {
		cfw, _ := w.Create(targetCoverHref)
		newCover, _ := os.ReadFile(newCoverPath)
		cfw.Write(newCover)
	}

	if len(newillusPath) > 0 {
		if !hasIllusPage {
			cfw, _ := w.Create(illusPagePath)
			cfw.Write([]byte(illusPageContent))
		}
		// 写入插画图片
		for i, imgPath := range newillusPath {
			imgID := strings.Split(strings.Split(illusManifestItems[i], `id="`)[1], `"`)[0]
			imgZipPath := ""
			// 根据ID反推图片路径写入
			for _, item := range illusManifestItems {
				if strings.Contains(item, imgID) {
					re := regexp.MustCompile(`href="([^"]+)"`)
					href := re.FindStringSubmatch(item)[1]
					imgZipPath = path.Join(opfDir, href)
				}
			}
			cfw, _ := w.Create(imgZipPath)
			data, _ := os.ReadFile(imgPath)
			cfw.Write(data)
		}
	}

	if err := w.Close(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	r.Close()

	return moveFile(outputPath, inputPath)
}

func epubToTxt(inputPath, outputPath string) {
	reader, err := zip.OpenReader(inputPath)
	if err != nil {
		fmt.Printf("✘ 文件读取失败: %v", err)
		return
	}
	defer reader.Close()

	var fullText strings.Builder
	fmt.Println("⏳ 正在提取内容...")

	for _, f := range reader.File {
		if !strings.HasSuffix(f.Name, ".xhtml") && !strings.HasSuffix(f.Name, ".html") {
			continue
		}

		rc, err := f.Open()
		if err != nil {
			continue
		}

		doc, err := goquery.NewDocumentFromReader(rc)
		rc.Close()
		if err != nil {
			continue
		}

		doc.Find("h1, h2, h3, h4, h5, h6, p, pre, div, span, data, time, dfn").Each(func(i int, s *goquery.Selection) {
			parentTag := ""
			if p := s.Parent(); p != nil {
				parentTag = goquery.NodeName(p)
			}
			if (parentTag == "p" || parentTag == "div" || parentTag == "pre") && (s.Is("span") || s.Is("data") || s.Is("time") || s.Is("dfn")) {
				return
			}

			tagName := goquery.NodeName(s)
			text := strings.TrimSpace(s.Text())
			if text == "" {
				return
			}

			switch tagName {
			case "h1", "h2", "h3", "h4", "h5", "h6":
				fmt.Printf("§ 识别到章节: %s\n", text)
				fullText.WriteString(fmt.Sprintf("\n %s \n", text))
			case "pre":
				fullText.WriteString("\n--- CODE BLOCK START ---\n")
				fullText.WriteString(s.Text())
				fullText.WriteString("\n--- CODE BLOCK END ---\n\n")
			default:
				fullText.WriteString(text + "\n\n")
			}
		})
	}

	if err := os.WriteFile(outputPath, []byte(fullText.String()), 0644); err != nil {
		fmt.Printf("✘ 生成失败: %v", err)
	} else {
		fmt.Printf("✔ 转换成功: %s\n", outputPath)
	}
}

func txtToEpub(inputPath, outputPath, chapterReg, coverPath, cssPath string, illusPath []string, llm string, wait int, htime bool, meta map[string]string, proxy string) {

	contentBytes, err := os.ReadFile(inputPath)
	if err != nil {
		fmt.Printf("✘ 文件读取失败: %v", err)
		return
	}

	decodedContent := autoDecode(contentBytes)

	if l, ok := meta["language"]; !ok || l == "" {
		meta["language"] = "zh-CN"
	}

	e, _ := epub.NewEpub(meta["title"])
	delete(meta, "title")
	for k, v := range meta {
		switch k {
		case "creator":
			e.SetAuthor(v)
			delete(meta, k)
		case "description":
			e.SetDescription(v)
			delete(meta, k)
		case "language":
			e.SetLang(v)
			delete(meta, k)
		}
	}

	if coverPath == "" {
		prelen := 30
		if len(decodedContent) < prelen {
			prelen = len(decodedContent)
		}
		coverPath = searchCoverLink(strings.Split(decodedContent, "\n")[:prelen])
		if coverPath != "" {
			fmt.Printf("ℹ 正文中识别到封面图链接: %s\n", coverPath)
		}
	}
	if coverPath != "" {
		if !fileExist(coverPath) {
			coverPath, _ = downloadImage(coverPath, proxy)
			if !fileExist(coverPath) {
				fmt.Printf("✘ 封面文件 %s 不存在或无权限\n", coverPath)
			} else {
				fmt.Printf("ℹ 封面文件 %s 下载成功\n", coverPath)
			}
		}
		if fileExist(coverPath) {
			internalCoverPath, err := e.AddImage(coverPath, "cover"+filepath.Ext(coverPath))
			if err != nil {
				fmt.Printf("✘ 封面文件 %s 添加失败: %v\n", coverPath, err)
			}
			if err := e.SetCover(internalCoverPath, ""); err != nil {
				fmt.Printf("✘ 封面文件 %s 配置失败: %v\n", coverPath, err)
			}
		}
	}

	if len(illusPath) > 0 {
		var htmlBuilder strings.Builder
		htmlBuilder.WriteString(`<div class="illustration-container" style="text-align: center;">`)
		for _, p := range illusPath {
			if !fileExist(p) {
				fmt.Printf("✘ 插图文件 %s 不存在或无权限\n", p)
				continue
			}

			internalPath, err := e.AddImage(p, "")
			if err != nil {
				fmt.Printf("✘ 插图文件 %s 添加失败: %v\n", p, err)
				continue
			}

			htmlBuilder.WriteString(fmt.Sprintf(`
			<div style='margin-bottom: 20px; text-align: center; margin: 1em 0; page-break-inside: avoid; page-break-after: auto;'>
				<img src='%s' style='max-width: 100%%; height: auto; display: block; margin: 0 auto;' />
			</div>`, internalPath))
		}

		htmlBuilder.WriteString(`</div>`)

		if _, err = e.AddSection(htmlBuilder.String(), "Illustrations", "illustration.xhtml", ""); err != nil {
			fmt.Printf("✘ 插画页面添加失败: %v\n", err)
		}
	}

	var internalCssPath string
	if cssPath != "" {
		if !fileExist(cssPath) {
			fmt.Printf("✘ 样式文件 %s 不存在或无权限\n", cssPath)
		}
		internalCssPath, err = e.AddCSS(cssPath, "style.css")
		if err != nil {
			fmt.Printf("✘ 样式文件 %s 添加失败: %v\n", cssPath, err)
		}
	} else {
		css := `
		body { font-family: "PingFang SC", "Hiragino Sans GB", "Microsoft YaHei", "Noto Serif CJK SC", serif; line-height: 1.85; padding: 5% 10%; text-align: justify; color: #333; }
		h2 { text-align: center; color: #1a5276; margin: 2.5em 0 1.5em 0; font-weight: 600; letter-spacing: 0.1em; }
		p { text-indent: 2em; hyphens: auto; color: #2c3e50; }
		span { color: #0e6251 !important; font-style: normal; letter-spacing: 0.05em; border-radius: 3px;}
		data { color: #7b1fa2 !important; font-style: normal; padding: 0 2px; letter-spacing: 0.05em; }
		dfn { color: #2e5984; letter-spacing: 0.05em; border-bottom: 1px solid rgba(22, 96, 96, 0.2); }
		time { color: #b45309; font-size: 0.95em; font-variant-numeric: tabular-nums; border-bottom: 1px dotted #aed6f1; }
		pre { overflow-x: auto; background-color: #f8f8f8; padding: 10px; border-radius: 4px; font-size: 0.85em; line-height: 1.5; font-family: "Courier New", monospace; margin: 1em 0; white-space: pre; word-wrap: normal; }
	`
		// 		cssContent := `
		//     body { font-family: sans-serif; line-height: 1.6; padding: 5%; }
		// 		h2 { text-align: center; margin: 1em 0; }
		// 		p { text-indent: 2em; margin: 0.5em 0; }
		// 		span { font-weight: bold; }
		//     .chapter-title { text-align: center; color: #2c3e50; border-bottom: 2px solid #eee; margin-bottom: 2em; }
		//     .dialogue { color: #e67e22; font-weight: bold; }
		//     .text-para { text-indent: 2em; margin: 0.5em 0; color: #333; }
		//		.code-wrapper pre { overflow-x: auto; background-color: #f8f8f8; padding: 10px; border-radius: 4px; font-size: 0.85em; line-height: 1.4; font-family: "Courier New", monospace; margin: 1em 0; white-space: pre; word-wrap: normal; }
		// `
		tempCss, _ := os.CreateTemp("", "style*.css")
		defer os.Remove(tempCss.Name())

		tempCss.WriteString(css)
		tempCss.Close()

		internalCssPath, _ = e.AddCSS(tempCss.Name(), "style.css")
	}

	buildEpub(e, decodedContent, chapterReg, internalCssPath, llm, wait, htime)

	if err := e.Write(outputPath); err != nil {
		fmt.Printf("✘ 生成失败: %v", err)
	} else {
		if len(meta) > 0 {
			if err := metaEdit(outputPath, "", "", nil, meta, proxy); err != nil {
				fmt.Printf("✘ 注入元数据失败: %v", err)
				return
			}
		}
		fmt.Printf("✔ 转换成功: %s\n", outputPath)
	}
}

func buildEpub(e *epub.Epub, content, chapterReg, cssPath, llm string, wait int, htime bool) {
	reDialog := regexp.MustCompile(`(?s)([“"‘'「].+?[”"’'」])`)
	reBook := regexp.MustCompile(`([『《〈【〔（({\[].+?[』》〉〕】）)}\]])`)
	reTime := regexp.MustCompile(`(\d{1,4}年)?\d{1,2}月\d{1,2}[日号號]|\d{1,2}[:：]\d{2}|[子丑寅卯辰巳午未申酉戌亥][时時]|第?[0-9一二三四五六七八九十百千万亿億两兩零壹贰叁肆伍陆陸柒漆捌玖拾佰仟萬数]{1,9}([点點][钟鐘整]|[分秒刻][钟鐘]?|[个個]?小[时時]|[个個]?[钟鐘][头頭]|[个個]?[时時]辰|[更天夜日周週月年岁歲]|[个個]?星期|[个個]?礼拜|[个個]?[季年]度|[个個]?[岁歲]?月|[个個]?年[头頭]?|甲子|代)|(?i)(?:[清凌]晨|[拂破][晓曉]|早[晨间間]|[上中下]午|午[间間后後]|傍晚|黄昏|薄暮|日落|[入深午子半]夜|整[日天]|白[日天]|晝間|大?[前作今本当當明后後隔][日天]|[翌隔次]日|[上中下]旬|[春夏秋冬][天季]|初春|早春|仲夏|中秋|深秋|秋后|隆冬|立春|雨水|惊蛰|驚蟄|春分|清明|[谷穀]雨|立夏|小[满滿]|芒[种種]|夏至|小暑|大暑|立秋|[处處]暑|白露|秋分|寒露|霜降|立冬|小雪|大雪|冬至|小寒|大寒|大?[去前今本当當明后後隔]年|[上下本当當][月周週]|[周週][一二三四五六壹贰叁肆伍陆陸日天末]|礼拜[一二三四五六壹贰叁肆伍陆陸日天]|星期[一二三四五六壹贰叁肆伍陆陸日天])`)

	// jieba := gojieba.NewJieba()

	lines := strings.Split(content, "\n")

	prefetch := 5000
	if len(lines) < prefetch {
		prefetch = len(lines)
	}
	chapterRegs := lockRegex(lines[:prefetch], chapterReg)

	var currentBody strings.Builder
	title := "前言"
	currentBody.WriteString(fmt.Sprintf("<h2 class='chapter'>%s</h2>\n", title))

	var inCodeBlock bool
	var codeBuffer strings.Builder
	var codeLanguage string

	for idx, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "```") {
			if !inCodeBlock {
				inCodeBlock = true
				codeLanguage = strings.TrimPrefix(line, "```")
				if codeLanguage == "" {
					codeLanguage = "text"
				}
			} else {
				highlighted := highlightCode(codeBuffer.String(), codeLanguage)
				currentBody.WriteString(fmt.Sprintf("<div class='code'>%s</div>", highlighted))
				codeBuffer.Reset()
				inCodeBlock = false
			}
			continue
		}

		if inCodeBlock {
			codeBuffer.WriteString(line + "\n")
		} else {
			if isChapter(line, chapterRegs) {
				if currentBody.Len() > 0 {
					e.AddSection(wrap(currentBody.String()), title, "", cssPath)
					currentBody.Reset()
				}
				title = line
				aiflag := ""
				if trueTitle(title) == "" && llm != "" {
					cend := idx + 1000
					if cend > len(lines) {
						cend = len(lines)
					}
					content := strings.TrimSpace(extractChapterContent(lines[idx:cend], chapterRegs))
					if ai, aititle := getAiTitle(llm, content, wait); aititle != "" {
						title = fmt.Sprintf("%s %s", title, aititle)
						aiflag = ai
					}
				}
				title = strings.TrimSpace(title)
				fmt.Printf("§ %s识别到章节: %s\n", aiflag, title)
				currentBody.WriteString(fmt.Sprintf("<h2 class='chapter'>%s</h2>\n", title))
			} else {
				processedLine := reDialog.ReplaceAllString(line, `<span class='dialog'>$1</span>`)
				processedLine = reBook.ReplaceAllString(processedLine, `<data class='book'>$1</data>`)
				if htime {
					processedLine = reTime.ReplaceAllString(processedLine, `<time class='time'>$0</time>`)
				}
				// tags := jieba.Tag(processedLine)
				// for _, tagStr := range tags {
				// 	parts := strings.Split(tagStr, "/")
				// 	word, tag := parts[0], parts[1]
				// 	switch tag {
				// 	case "nr":
				// 		processedLine = strings.ReplaceAll(processedLine, word, fmt.Sprintf(`<dfn class="name">%s</dfn>`, word))
				// 	case "ns":
				// 		processedLine = strings.ReplaceAll(processedLine, word, fmt.Sprintf(`<dfn class="place">%s</dfn>`, word))
				// 	case "nt":
				// 		processedLine = strings.ReplaceAll(processedLine, word, fmt.Sprintf(`<dfn class="organization">%s</dfn>`, word))
				// 	case "nz":
				// 		processedLine = strings.ReplaceAll(processedLine, word, fmt.Sprintf(`<dfn class="entity">%s</dfn>`, word))
				// 	}
				// }
				currentBody.WriteString(fmt.Sprintf("<p class='text'>%s</p>\n", processedLine))
			}
		}
	}
	e.AddSection(wrap(currentBody.String()), title, "", cssPath)
}

func getMediaType(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	default:
		return "image/jpeg"
	}
}

func uuid() string {
	uuid := make([]byte, 16)
	rand.Read(uuid)

	// 设置版本号 (4) 和变体 (RFC 4122)
	uuid[6] = (uuid[6] & 0x0f) | 0x40 // Version 4
	uuid[8] = (uuid[8] & 0x3f) | 0x80 // Variant 10xx

	return fmt.Sprintf("%x-%x-%x-%x-%x",
		uuid[0:4], uuid[4:6], uuid[6:8], uuid[8:10], uuid[10:16])
}
func fileExist(path string) bool {
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	return false
}

func moveFile(src, dst string) error {
	err := os.Rename(src, dst)
	if err == nil {
		return nil
	}

	source, err := os.Open(src)
	if err != nil {
		return err
	}
	defer source.Close()

	destination, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer destination.Close()

	if _, err := io.Copy(destination, source); err != nil {
		return err
	}

	source.Close()
	destination.Close()

	return os.Remove(src)
}

func ensureMetadataTag(xmlStr, tagName, tagValue string) string {
	if tagValue == "" {
		return xmlStr
	}

	if tagName == "creator" || tagName == "contributor" || tagName == "subject" {
		re := regexp.MustCompile(fmt.Sprintf(`(?s)(<(?:\w+:)?%s[^>]*>).*?(</(?:\w+:)?%s>)`, tagName, tagName))
		if re.MatchString(xmlStr) {
			return re.ReplaceAllString(xmlStr, "")
		}
		var newNode string
		reMeta := regexp.MustCompile(`(?i)(<metadata[^>]*>)`)
		for _, tagVal := range strings.Split(tagValue, ",") {
			newNode += fmt.Sprintf("\n    <dc:%s>%s</dc:%s>", tagName, strings.TrimSpace(tagVal), tagName)
		}
		return reMeta.ReplaceAllString(xmlStr, "${1}"+newNode)
	}

	re := regexp.MustCompile(fmt.Sprintf(`(?s)(<(?:\w+:)?%s[^>]*>).*?(</(?:\w+:)?%s>)`, tagName, tagName))
	if re.MatchString(xmlStr) {
		return re.ReplaceAllString(xmlStr, "${1}"+tagValue+"${2}")
	}

	reMeta := regexp.MustCompile(`(?i)(<metadata[^>]*>)`)
	newNode := fmt.Sprintf("\n    <dc:%s>%s</dc:%s>", tagName, tagValue, tagName)

	return reMeta.ReplaceAllString(xmlStr, "${1}"+newNode)
}

func searchCoverLink(lines []string) string {
	link := ""
	for _, line := range lines {
		if matches := regexp.MustCompile(`(?i)^\s*(?:Cover|封面|封面图)[：:]\s*(https?://[^\s\n]+).*$`).FindStringSubmatch(line); len(matches) > 1 {
			return strings.TrimSpace(matches[1])
		}
	}
	return link
}

func lockRegex(lines []string, customRe string) []string {
	// 1. 定义内置正则库
	patterns := []string{
		// 1. 标准强特征型 (第x章)
		`^\s*第\s*[0-9一二三四五六七八九十百千万亿億两兩零壹贰叁肆伍陆陸柒漆捌玖拾佰仟萬]{1,9}\s*[章节節回迴卷折篇幕序番部季集段层層场場话話页頁记記说說志考述引曲]\s*.{0,30}$`,

		// 2. 标准弱特征型 (十二章)
		`^\s*[0-9一二三四五六七八九十百千万亿億两兩零壹贰叁肆伍陆陸柒漆捌玖拾佰仟萬]{1,9}\s*[章节節回迴卷折篇幕序番部季集段层層场場话話页頁记記说說志考述引曲]\s*.{0,30}$`,

		// 3. 符号包裹型 (【第一章】)
		`^\s*[\[《〈〈【『「〔（<({].{0,10}[章节節回迴卷折篇幕序番部季集段层層场場话話页頁记記说說志考述引曲].{0,10}[\]》〉〉〕」』】）>)}].{0,30}$`,

		// 4. 无符号数字型(天界篇)
		`^\s*[^第\s]{0,10}[章节節回迴卷折篇幕序番部季集段层層场場话話页頁记記说說志考述引曲]$`,

		// 5. 数字分隔型(二十一 标题)
		`^\s*[0-9一二三四五六七八九十百千万亿億两兩零壹贰叁肆伍陆陸柒漆捌玖拾佰仟萬]{1,9}[、.：:|｜——\s-]+.{0,30}$`,

		// 6. 符号包裹数字型([01] 标题)
		`^\s*[\[《〈〈【『「〔（<({][0-9一二三四五六七八九十百千万亿億两兩零壹贰叁肆伍陆陸柒漆捌玖拾佰仟萬]{1,9}[\]》〉〉〕」』】）>)}][、.：:|｜——\s-]?.{0,30}$`,

		// 4. 英文标题型(Chapter 1)
		`(?i)^\s*(Chapter|Section|Case|Episode|Lesson|Clause|Article|Book|Part|Unit|Stanza|Canto|Vol|Volume|Catalog|Preface|Foreword|Prologue|Abstract|Summary|Synopsis|Opening|Ending|Afterword|Epilogue|Interlude|Appendix|Acknowledgments|Postscript|Extra|Toc|Table of Contents|Related Information|Back Matter|Final Words| Closing Remarks|Side Story)\s*[0-9]{1,5}.{0,30}$`,

		// 7. 罗马数字型
		`(?i)^\s*(M{1,4}|M{0,4}(?:CM|CD|D?C{1,3})|M{0,4}(?:D?C{0,3})(?:XC|XL|L?X{1,3})|M{0,4}(?:D?C{0,3})(?:L?X{0,3})(?:IX|IV|V?I{1,3}))([、.：:|｜——\s-].{1,30})$`,
	}

	// 如果用户提供了自定义正则，优先级最高
	if customRe != "" {
		if length, err := strconv.Atoi(customRe); err == nil {
			fmt.Printf("¶ 启用用户自定义章节长度: <=%d\n", length)
		} else {
			fmt.Printf("¶ 启用用户自定义章节正则: %s\n", customRe)
		}
		return []string{customRe}
	}

	counts := make([]int, len(patterns))

	// 2. 统计匹配频率
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(strings.TrimPrefix(lines[i], "正文 "))
		line = trimLeftSymbols(line)
		if line == "" {
			continue
		}

		runeLine := []rune(line)
		if len(runeLine) > 40 || len(runeLine) < 2 {
			continue
		}
		lastChar := runeLine[len(runeLine)-1]
		if strings.ContainsRune("，。：”》", lastChar) {
			continue
		}

		for idx, p := range patterns {
			re := regexp.MustCompile(p)
			if re.MatchString(line) && len([]rune(line)) < 50 {
				counts[idx]++
			}
		}
	}

	// 3. 锁定逻辑：寻找匹配次数最多的
	maxIdx := -1
	maxCount := 0
	totalMatches := 0
	for i, c := range counts {
		totalMatches += c
		if c > maxCount {
			maxCount = c
			maxIdx = i
		}
	}

	// 阈值判断：如果最强的正则贡献了超过 70% 的匹配，则锁定它
	if maxIdx != -1 && float64(maxCount)/float64(totalMatches) > 0.7 {
		fmt.Printf("¶ 启用启发式锁定章节正则: %s\n", patterns[maxIdx])
		return []string{patterns[maxIdx]}
	}

	fmt.Println("¶ 启用内置多模式章节正则: ")
	for i, c := range counts {
		fmt.Printf("%s -> %d\n", patterns[i], c)
	}
	return patterns
}

func trueTitle(str string) string {
	title := strings.TrimSpace(strings.TrimPrefix(str, "正文 "))
	for _, s := range []string{"序", "序言", "序章", "前言", "后记", "後記", "番外", "引子", "前言", "自序", "终章", "終章", "结局", "結局", "结语", "結语", "结語", "結語", "楔子", "致谢", "致謝", "目录", "目錄", "附录", "附錄", "简介", "簡介", "内容简介", "內容簡介", "作品相关", "作品相關", "写在最后", "寫在最後", "Catalog", "Preface", "Foreword", "Prologue", "Abstract", "Summary", "Synopsis", "Opening", "Ending", "Afterword", "Epilogue", "Interlude", "Appendix", "Acknowledgments", "Postscript", "Extra", "Toc", "Table of Contents", "Related Information", "Back Matter", "Final Words", " Closing Remarks", "Side Story"} {
		if strings.HasPrefix(strings.ToLower(title), strings.ToLower(s)) {
			return str
		}
	}

	var builder strings.Builder
	builder.Grow(len(title))

	for _, r := range title {
		if unicode.IsLetter(r) && !strings.Contains("一二三四五六七八九十百千万亿億两兩零壹贰叁肆伍陆陸柒漆捌玖拾佰仟萬", string(r)) {
			builder.WriteRune(r)
		}
	}
	title = builder.String()

	pattern := `(?i)章|节|節|回|迴|卷|折|篇|幕|序|番|部|季|集|段|层|層|场|場|话|話|页|頁|记|記|说|說|志|考|述|引|曲|Chapter|Section|Case|Episode|Lesson|Clause|Article|Book|Part|Unit|Stanza|Canto|Vol|Volume`
	re := regexp.MustCompile(pattern)
	titles := re.Split(title, 2)
	if len(titles) > 1 {
		if strings.TrimSpace(strings.TrimPrefix(titles[0], "第")) != "" {
			return str
		} else {
			return titles[1]
		}
	}
	return title
}

func isChapter(line string, regexps []string) bool {
	line = strings.TrimSpace(strings.TrimPrefix(line, "正文 "))
	line = trimLeftSymbols(line)
	if line == "" {
		return false
	}

	runeLine := []rune(line)
	if len(runeLine) > 40 || len(runeLine) < 2 {
		return false
	}
	// 前言后记等特殊章节直接识别
	if matched, _ := regexp.MatchString(`(?m)^\s*(序|序章|序言|前言|后记|後記|番外|引子|前言|自序|终章|終章|结局|結局|结语|結语|结語|結語|楔子|致谢|致謝|目录|目錄|附录|附錄|简介|簡介|内容简介|內容簡介|作品相关|作品相關|写在最后|寫在最後)\s*$`, line); matched {
		return true
	}

	lastChar := runeLine[len(runeLine)-1]
	if strings.ContainsRune("，。：”’》」；'\",;", lastChar) {
		return false
	}

	for _, reStr := range regexps {
		if length, err := strconv.Atoi(reStr); err == nil {
			if len(runeLine) <= length {
				return true
			}
		} else {
			matched, _ := regexp.MatchString(reStr, line)
			if matched {
				return true
			}
		}
	}
	return false
}

func trimLeftSymbols(s string) string {
	return strings.TrimLeftFunc(s, func(r rune) bool {
		// 如果字符不是字母且不是数字，就返回 true（表示需要被 trim 掉）
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}

func extractChapterContent(lines []string, regexps []string) string {
	foundFirst := false
	var start, end int
	for idx, line := range lines {
		if isChapter(line, regexps) {
			if !foundFirst {
				foundFirst = true
				start = idx
				continue
			} else {
				end = idx
				break
			}
		}
	}
	if end == 0 {
		end = len(lines)
	}
	if start < end {
		return strings.Join(lines[start+1:end], "\n")
	} else {
		return ""
	}
}

func autoDecode(data []byte) string {
	detector := chardet.NewTextDetector()
	result, err := detector.DetectBest(data)
	if err != nil {
		return string(data)
	}
	normalizedCharset := strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(result.Charset, "-", ""), " ", ""))

	fmt.Printf("⚲ 检测到编码: %s (置信度: %d%%)\n", normalizedCharset, result.Confidence)

	var decoder transform.Transformer

	switch normalizedCharset {
	case "GB18030", "GBK", "GB2312":
		decoder = simplifiedchinese.GB18030.NewDecoder()
	case "BIG5":
		decoder = traditionalchinese.Big5.NewDecoder()
	case "UTF8":
		return string(data)
	default:
		if result.Confidence < 50 {
			decoder = simplifiedchinese.GB18030.NewDecoder()
		} else {
			return string(data)
		}
	}

	reader := transform.NewReader(bytes.NewReader(data), decoder)
	decoded, err := io.ReadAll(reader)
	if err != nil {
		return string(data)
	}

	return string(decoded)

}

func highlightCode(code, language string) string {
	// 1. 准备 Lexer (语法解析器)
	lexer := lexers.Get(language)
	if lexer == nil {
		lexer = lexers.Fallback
	}
	lexer = chroma.Coalesce(lexer)

	// 2. 选择 Style (配色方案)
	// 推荐: "monokai" (深色), "friendly" (浅色), "github"
	style := styles.Get("friendly")
	if style == nil {
		style = styles.Fallback
	}

	// 3. 准备 Formatter (输出格式)
	// WithClasses(false) 会生成内联 style="..." 属性，这对 EPUB 最安全
	formatter := chtml.New(chtml.WithClasses(false), chtml.TabWidth(4))

	// 4. 执行渲染
	var buf bytes.Buffer
	iterator, err := lexer.Tokenise(nil, code)
	if err != nil {
		return code
	}

	err = formatter.Format(&buf, style, iterator)
	if err != nil {
		return code
	}

	return buf.String()
}

func wrap(body string) string {
	return fmt.Sprintf("<body>%s</body>", body)
}

func downloadImage(imgURL, proxy string) (string, error) {
	var proxyFunc func(*http.Request) (*url.URL, error)

	if proxy != "" {
		proxyURL, err := url.Parse(proxy)
		if err != nil {
			return "", fmt.Errorf("代理地址格式错误: %v", err)
		}
		proxyFunc = http.ProxyURL(proxyURL)
	} else {
		proxyFunc = http.ProxyFromEnvironment
	}

	transport := &http.Transport{
		Proxy:           proxyFunc,
		IdleConnTimeout: 90 * time.Second,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}

	resp, err := client.Get(imgURL)
	if err != nil {
		return "", fmt.Errorf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("服务器返回状态码: %d", resp.StatusCode)
	}

	fileName := filepath.Base(imgURL)
	if fileName == "." || fileName == "/" {
		fileName = "cover.jpg" // 兜底文件名
	}

	out, err := os.Create(fileName)
	if err != nil {
		return "", fmt.Errorf("无法创建文件: %v", err)
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	if err != nil {
		return "", fmt.Errorf("保存内容失败: %v", err)
	}

	absPath, _ := filepath.Abs(fileName)
	return absPath, nil
}

func crawl(upath, rate, proxy string, retry int) (string, error) {
	var filename string
	u, err := url.Parse(upath)
	if err != nil {
		fmt.Printf("✘ 无法识别为有效的URL: %v\n", err)
		return filename, err
	}
	spider := NewSpider(rate, proxy, retry)
	fmt.Printf("ℹ 正在爬取简介页: %s ... ", upath)

	switch u.Hostname() {
	case "alicesw.com", "www.alicesw.com":
		content, err := spider.Fetch(upath)
		if err != nil {
			fmt.Printf("[错误: %v]\n", err)
			return filename, err
		}
		doc, err := goquery.NewDocumentFromReader(strings.NewReader(content))
		if err != nil {
			fmt.Printf("[错误: %v]\n", err)
			return filename, err
		}

		info := doc.Find("div.box_intro")

		name := strings.TrimSpace(info.Find("div.box_info div.novel_title").Text())
		author := strings.TrimSpace(info.Find("div.box_info div.novel_info p").Eq(0).Find("a").Text())
		cata := strings.TrimSpace(info.Find("div.box_info div.novel_info p").Eq(1).Find("a").Text())
		status := strings.TrimSpace(strings.TrimPrefix(info.Find("div.box_info div.novel_info p").Eq(4).Text(), "状 态："))
		image := info.Find("div.pic img").AttrOr("src", "")

		if name == "" {
			fmt.Printf("[错误: 获取简介失败]\n")
			return filename, fmt.Errorf("empty")
		} else {
			fmt.Printf("[成功]\n")
		}

		filename = fmt.Sprintf("%s.txt", name)
		write(filename, fmt.Sprintf("%s\n\n作者：%s\n分类：%s\n状态：%s\n封面：%s\n--------------------------------------------------\n", name, author, cata, status, image), true)

		curlpath, _ := url.JoinPath(fmt.Sprintf("%s://%s", u.Scheme, u.Host), "/other/chapters/id/", path.Base(u.Path))

		fmt.Printf("ℹ 正在爬取章节页: [%s] %s ... ", name, curlpath)
		ccontent, err := spider.Fetch(curlpath)
		if err != nil {
			fmt.Printf("[错误: %v]\n", err)
			return filename, err
		}
		cdoc, err := goquery.NewDocumentFromReader(strings.NewReader(ccontent))
		if err != nil {
			fmt.Printf("[错误: %v]\n", err)
			return filename, err
		}

		if strings.TrimSpace(cdoc.Find("div.mu_h1 h1").Text()) == "" {
			fmt.Printf("[错误: 获取章节失败]\n")
			return filename, fmt.Errorf("empty")
		} else {
			fmt.Printf("[成功]\n")
		}

		cdoc.Find("ul.mulu_list li").Each(func(i int, s *goquery.Selection) {
			chapterTitle := strings.TrimSpace(s.Find("a").Text())
			chapterLink := s.Find("a").AttrOr("href", "")

			surlpath, _ := url.JoinPath(fmt.Sprintf("%s://%s", u.Scheme, u.Host), chapterLink)

			fmt.Printf("ℹ 正在爬取内容页: [%s] %s ... ", chapterTitle, surlpath)
			scontent, err := spider.Fetch(surlpath)
			if err == nil {
				sdoc, err := goquery.NewDocumentFromReader(strings.NewReader(scontent))
				if err == nil {
					if strings.TrimSpace(sdoc.Find("div.text-head h3").Text()) == "" {
						fmt.Printf("[错误: 获取内容失败]\n")
					} else {
						fmt.Printf("[成功]\n")
						write(filename, chapterTitle+"\n", false)
						sdoc.Find("div.read-content p").Each(func(i int, s *goquery.Selection) {
							write(filename, s.Text()+"\n", false)
						})
					}
				} else {
					fmt.Printf("[错误: %v]\n", err)
				}
			} else {
				fmt.Printf("[错误: %v]\n", err)
			}
		})
	}
	return filepath.Abs(filename)
}

func write(filename string, message string, show bool) error {
	// 1. 打开文件（如果不存在则创建，追加模式）
	// os.O_APPEND: 追加写入
	// os.O_CREATE: 文件不存在则创建
	// os.O_WRONLY: 只写模式
	f, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("无法打开文件: %v", err)
	}
	defer f.Close()

	// 2. 创建 MultiWriter，组合标准输出 (Stdout) 和文件 (f)
	mw := io.MultiWriter(os.Stdout, f)
	if !show {
		mw = io.MultiWriter(f)
	}

	// 3. 向 MultiWriter 写入，它会自动分发到两个地方
	_, err = fmt.Fprintln(mw, message)
	return err
}

func getAiTitle(llm, chapterContent string, wait int) (string, string) {
	chapterContent = strings.TrimSpace(chapterContent)
	if chapterContent == "" {
		return "", ""
	}

	var llmurl, apikey string
	var models []string
	llminfo := strings.Split(llm, ":")
	if len(llminfo) < 2 {
		return "", ""
	}

	llmname := strings.ToLower(llminfo[0])
	apikey = llminfo[1]
	if llmname == "" || apikey == "" {
		return "", ""
	}

	llmkey := strings.Split(llmname, "/")
	llmname = llmkey[0]
	if len(llmkey) > 1 {
		models = strings.Split(strings.Join(llmkey[1:], "/"), ",")
	}

	switch llmname {
	case "glm":
		llmurl = "https://open.bigmodel.cn/api/paas/v4/chat/completions"
		if len(models) == 0 {
			models = []string{"glm-4-flash"}
		}
	case "gemini":
		llmurl = "https://api.gemini.com/v1/chat/completions"
		if len(models) == 0 {
			models = []string{"gemini-flash-lite-latest"}
		}
	case "sili", "silicon", "siliconflow":
		llmurl = "https://api.siliconflow.cn/v1/chat/completions"
		if len(models) == 0 {
			models = []string{"Qwen/Qwen3-8B", "Qwen/Qwen2.5-7B-Instruct", "Qwen/Qwen2-7B-Instruct", "deepseek-ai/DeepSeek-R1-0528-Qwen3-8B", "deepseek-ai/DeepSeek-R1-Distill-Qwen-7B", "Qwen/Qwen3.5-4B", "THUDM/GLM-4.1V-9B-Thinking", "THUDM/glm-4-9b-chat", "THUDM/GLM-Z1-9B-0414", "THUDM/GLM-4-9B-0414", "tencent/Hunyuan-MT-7B"}
		}
	default:
		return "", ""
	}

	var output string

	for {
		for _, model := range models {
			payload := map[string]interface{}{
				"model": model,
				"messages": []map[string]string{
					{"role": "system", "content": "你是一位优秀的网文编辑，请根据正文生成一个2-7字的简炼直观契合风格的章节标题，严格遵守字数限制，只生产一个最合适的，不能包含符号，只允许返回标题文本。"},
					{"role": "user", "content": "内容如下：\n" + chapterContent},
				},
			}

			body, _ := json.Marshal(payload)

			req, _ := http.NewRequest("POST", llmurl, bytes.NewBuffer(body))
			req.Header.Set("Authorization", "Bearer "+apikey)
			req.Header.Set("Content-Type", "application/json")

			for i := 2; i > 0; i-- {
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					fmt.Printf("✘ 请求大模型 %s(%s) 失败: %v\n", llmname, model, err)
					time.Sleep(time.Duration(wait) * time.Millisecond)
					continue
				}
				defer resp.Body.Close()

				respData, _ := io.ReadAll(resp.Body)
				var result map[string]interface{}
				if err := json.Unmarshal(respData, &result); err != nil {
					fmt.Printf("✘ 解析大模型 %s(%s) 响应失败: %v\n", llmname, model, err)
					time.Sleep(time.Duration(wait) * time.Millisecond)
					continue
				}

				if choices, ok := result["choices"].([]interface{}); ok && len(choices) > 0 {
					firstChoice := choices[0].(map[string]interface{})
					message := firstChoice["message"].(map[string]interface{})
					output = strings.TrimSpace(message["content"].(string))
					break
				} else {
					fmt.Printf("✘ 大模型 %s(%s) 响应异常: %v\n", llmname, model, string(respData))
					time.Sleep(time.Duration(wait) * time.Millisecond)
					continue
				}
			}

			if output != "" {
				return model, regexp.MustCompile(`[^\p{L}\p{N}]+`).ReplaceAllString(output, "")
			}
		}
		time.Sleep(time.Duration(60000/wait/len(models)+1) * time.Second)
	}
}
