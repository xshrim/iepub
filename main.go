package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
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
	ChapterRe   string
	ImgRe       string // 图片嵌入正则，例如 [IMG:1.jpg]
	Llm         string // 大模型配置，例如 glm/glm-4-flash:xxxx
	Wait        int    // 大模型调用毫秒间隔
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

func main() {
	cfg := AdvancedConfig{}
	flag.BoolVar(&cfg.Info, "i", false, "获取或修改epub元数据")
	flag.StringVar(&cfg.OutputPath, "o", "", "输出文件(默认: 输入文件名.epub)")
	flag.StringVar(&cfg.Title, "t", "", "书名(默认: 输入文件名)")
	flag.StringVar(&cfg.Creator, "a", "", "作者")
	flag.StringVar(&cfg.Contributor, "x", "", "制作者与协作者")
	flag.StringVar(&cfg.Language, "g", "", "语言")
	flag.StringVar(&cfg.Description, "e", "", "描述")
	flag.StringVar(&cfg.Subject, "k", "", "标签")
	flag.StringVar(&cfg.Date, "d", "", "发行日期")
	flag.StringVar(&cfg.Publisher, "u", "", "出版商")
	flag.StringVar(&cfg.Rights, "b", "", "版权声明")
	flag.StringVar(&cfg.CoverPath, "c", "", "封面图片路径")
	flag.StringVar(&cfg.CssPath, "s", "", "样式文件路径")
	flag.StringVar(&cfg.ChapterRe, "r", ``, "章节识别正则(默认: 内置自动检测规则)")
	flag.StringVar(&cfg.ImgRe, "m", `\[IMG:(.*?)\]`, "图片标签识别正则(默认: [IMG:xxx])")
	flag.BoolVar(&cfg.Htime, "v", false, "高亮时间")
	flag.StringVar(&cfg.Llm, "l", "", "大模型补全章节标题(格式: glm/glm-4-flash:xxxx)")
	flag.IntVar(&cfg.Wait, "w", 1000, "大模型调用毫秒间隔(默认: 1000ms)")
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
		fmt.Println("✘ 错误: 请提供输入文件")
		fmt.Println("用法示例: iepub input.txt 或 iepub input.epub")
		return
	}
	if _, err := os.Stat(cfg.InputPath); err != nil {
		fmt.Println("✘ 错误: 获取输入文件失败:", err.Error())
		return
	}

	switch strings.ToLower(filepath.Ext(filepath.Base(cfg.InputPath))) {
	case ".txt":
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
		txtToEpub(cfg.InputPath, cfg.OutputPath, cfg.ChapterRe, cfg.CoverPath, cfg.CssPath, cfg.Llm, cfg.Wait, cfg.Htime, meta)
	case ".epub":
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

			if len(meta) > 0 || cfg.CoverPath != "" {
				metaEdit(cfg.InputPath, cfg.OutputPath, cfg.CoverPath, meta)
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
	}
}

func server(port int) {
	http.HandleFunc("/convert", handleConvert)
	http.HandleFunc("/metadata", handleGetMetadata)
	http.HandleFunc("/edit", handleEditMetadata)

	fmt.Printf("▶ 启动服务模式: http://0.0.0.0:%d\n", port)
	http.ListenAndServe(fmt.Sprintf(":%d", port), nil)
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

	err = metaEdit(tempInput, tempOutput, newCoverPath, meta)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Disposition", "attachment; filename=modified_"+header.Filename)
	http.ServeFile(w, r, tempOutput)
	os.Remove(tempOutput)
}

func handleConvert(w http.ResponseWriter, r *http.Request) {
	file, header, _ := r.FormFile("file")
	ext := filepath.Ext(header.Filename)

	tempInput := filepath.Join(os.TempDir(), header.Filename)
	f, _ := os.Create(tempInput)
	io.Copy(f, file)
	f.Close()
	defer os.Remove(tempInput)

	var outputName string
	var targetPath string

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

	switch ext {
	case ".epub":
		outputName = strings.TrimSuffix(header.Filename, ".epub") + ".txt"
		targetPath = filepath.Join(os.TempDir(), outputName)
		epubToTxt(tempInput, targetPath)
	case ".txt":
		outputName = strings.TrimSuffix(header.Filename, ".txt") + ".epub"
		targetPath = filepath.Join(os.TempDir(), outputName)
		txtToEpub(tempInput, targetPath, chpre, newCoverPath, newCssPath, llm, wait, htime, meta)
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
					cover = path.Join(path.Dir(f.Name), item.Href)
					break
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

func metaEdit(inputPath, outputPath, newCoverPath string, meta map[string]string) error {
	if len(meta) == 0 && newCoverPath == "" {
		return nil
	}

	r, err := zip.OpenReader(inputPath)
	if err != nil {
		return err
	}

	var opfPath, opfContent, originalCoverHref string
	var opfDir string

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
					originalCoverHref = path.Join(opfDir, item.Href)
					break
				}
			}
		}
	}

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
		targetCoverHref = path.Join(opfDir, "cover"+filepath.Ext(newCoverPath))
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
					path.Base(targetCoverHref), getMediaType(newCoverPath))
				modified = reManiTag.ReplaceAllString(modified, "${1}"+itemNode)
			}

			fw.Write([]byte(modified))
		} else if newCoverPath != "" && f.Name == targetCoverHref {
			newCover, _ := os.ReadFile(newCoverPath)
			fw.Write(newCover)
			coverWritten = true
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

func txtToEpub(inputPath, outputPath, chapterReg, coverPath, cssPath, llm string, wait int, htime bool, meta map[string]string) {

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

	if coverPath != "" {
		internalCoverPath, _ := e.AddImage(coverPath, "cover.jpg")
		e.SetCover(internalCoverPath, "")
	}

	var internalCssPath string
	if cssPath != "" {
		internalCssPath, _ = e.AddCSS(cssPath, "style.css")
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
			if err := metaEdit(outputPath, "", "", meta); err != nil {
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
	reTime := regexp.MustCompile(`(\d{1,4}年)?\d{1,2}月\d{1,2}[日号號]|\d{1,2}[:：]\d{2}|[子丑寅卯辰巳午未申酉戌亥][时時]|第?[0-9一二三四五六七八九十百千万亿億两兩零壹贰叁肆伍陆陸柒漆捌玖拾佰仟萬数]{1,9}([点點][钟鐘整]|[分秒刻][钟鐘]?|[个個]?小[时時]|[个個]?[钟鐘][头頭]|[个個]?[时時]辰|[更天夜日周週月年]|[个個]?星期|[个個]?[季年]度|[个個]?[岁歲]?月|[个個]?年[头頭]?|甲子|代)|(?i)(?:[清凌]晨|[拂破][晓曉]|早[晨间間]|[上中下]午|午[间間后後]|傍晚|黄昏|薄暮|日落|[入深午子半]夜|整[日天]|白[日天]|晝間|大?[前作今本当當明后後隔][日天]|[翌隔次]日|[上中下]旬|[春夏秋冬][天季]|初春|早春|仲夏|中秋|深秋|秋后|隆冬|立春|雨水|惊蛰|驚蟄|春分|清明|[谷穀]雨|立夏|小[满滿]|芒[种種]|夏至|小暑|大暑|立秋|[处處]暑|白露|秋分|寒露|霜降|立冬|小雪|大雪|冬至|小寒|大寒|大?[去前今本当當明后後隔]年|[上下本当當][月周週]|[周週][一二三四五六壹贰叁肆伍陆陸日天末]|礼拜[一二三四五六壹贰叁肆伍陆陸日天]|星期[一二三四五六壹贰叁肆伍陆陸日天])`)

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
		fmt.Printf("¶ 启用用户自定义章节正则: %s\n", customRe)
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
	for _, s := range []string{"序言", "前言", "后记", "後記", "番外", "引子", "前言", "自序", "终章", "終章", "结局", "結局", "楔子", "致谢", "致謝", "目录", "目錄", "附录", "附錄", "简介", "簡介", "内容简介", "內容簡介", "作品相关", "作品相關", "写在最后", "寫在最後", "Catalog", "Preface", "Foreword", "Prologue", "Abstract", "Summary", "Synopsis", "Opening", "Ending", "Afterword", "Epilogue", "Interlude", "Appendix", "Acknowledgments", "Postscript", "Extra", "Toc", "Table of Contents", "Related Information", "Back Matter", "Final Words", " Closing Remarks", "Side Story"} {
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
	if matched, _ := regexp.MatchString(`(?m)^\s*(序言|前言|后记|後記|番外|引子|前言|自序|终章|終章|结局|結局|楔子|致谢|致謝|目录|目錄|附录|附錄|简介|簡介|内容简介|內容簡介|作品相关|作品相關|写在最后|寫在最後)\s*$`, line); matched {
		return true
	}

	lastChar := runeLine[len(runeLine)-1]
	if strings.ContainsRune("，。：”》", lastChar) {
		return false
	}

	for _, reStr := range regexps {
		matched, _ := regexp.MatchString(reStr, line)
		if matched {
			return true
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
