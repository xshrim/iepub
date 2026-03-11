package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
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
	InputPath  string
	OutputPath string
	Title      string
	Author     string
	CoverPath  string
	CssPath    string
	ChapterRe  string
	ImgRe      string // 图片嵌入正则，例如 [IMG:1.jpg]
	Llm        string // 大模型配置，例如 glm/glm-4-flash:xxxx
	Wait       int    // 大模型调用毫秒间隔
}

func main() {
	cfg := AdvancedConfig{}
	flag.StringVar(&cfg.InputPath, "i", "", "输入TXT文件")
	flag.StringVar(&cfg.OutputPath, "o", "", "输出文件(默认: 输入文件名.epub)")
	flag.StringVar(&cfg.Title, "t", "", "书名(默认: 输入文件名)")
	flag.StringVar(&cfg.Author, "a", "Unknown", "作者(默认: Unknown)")
	flag.StringVar(&cfg.CoverPath, "c", "", "封面图片路径")
	flag.StringVar(&cfg.CssPath, "s", "", "样式文件路径")
	flag.StringVar(&cfg.ChapterRe, "r", ``, "章节识别正则(默认: 内置自动检测规则)")
	flag.StringVar(&cfg.ImgRe, "m", `\[IMG:(.*?)\]`, "图片标签识别正则(默认: [IMG:xxx])")
	flag.StringVar(&cfg.Llm, "l", "", "大模型补全章节标题(格式: glm/glm-4-flash:xxxx)")
	flag.IntVar(&cfg.Wait, "w", 300, "大模型调用毫秒间隔(默认: 300ms)")
	flag.Parse()

	if cfg.InputPath == "" && flag.NArg() > 0 {
		cfg.InputPath = flag.Arg(0)
	}
	if cfg.InputPath == "" {
		fmt.Println("❌ 错误: 请提供输入文件")
		fmt.Println("用法示例: iepub input.txt 或 iepub input.epub 或 iepub -i input.txt 或 iepub -i input.epub")
		return
	}
	if _, err := os.Stat(cfg.InputPath); err != nil {
		fmt.Println("❌ 错误: 获取输入文件失败:", err.Error())
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
		txtToepub(cfg.InputPath, cfg.OutputPath, cfg.Title, cfg.Author, cfg.ChapterRe, cfg.CoverPath, cfg.CssPath, cfg.Llm, cfg.Wait)
	case ".epub":
		if cfg.OutputPath == "" {
			cfg.OutputPath = strings.TrimSuffix(filepath.Base(cfg.InputPath), filepath.Ext(filepath.Base(cfg.InputPath))) + ".txt"
			fmt.Printf("ℹ 未指定输出文件名，自动使用输入文件名: [%s]\n", cfg.OutputPath)
		}
		epubToTxt(cfg.InputPath, cfg.OutputPath)
	}
}

func epubToTxt(inputPath, outputPath string) {
	reader, err := zip.OpenReader(inputPath)
	if err != nil {
		fmt.Printf("❌ 文件读取失败: %v", err)
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

		// 3. 提取逻辑：包含标题、段落、代码块、容器和跨度
		// 我们按顺序选择所有可能的文本容器
		doc.Find("h1, h2, h3, h4, h5, h6, p, pre, div, span").Each(func(i int, s *goquery.Selection) {
			// 避坑：如果 span 在 p 或 div 内部，直接取父节点的 Text 即可，防止重复提取
			parentTag := ""
			if p := s.Parent(); p != nil {
				parentTag = goquery.NodeName(p)
			}
			if (parentTag == "p" || parentTag == "div" || parentTag == "pre") && s.Is("span") {
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
				fullText.WriteString(fmt.Sprintf("\n\n【 %s 】\n\n", text))
			case "pre":
				// 保留代码块的原始换行
				fullText.WriteString("\n--- CODE BLOCK START ---\n")
				fullText.WriteString(s.Text())
				fullText.WriteString("\n--- CODE BLOCK END ---\n\n")
			default:
				// 普通段落、div 或 span
				fullText.WriteString(text + "\n\n")
			}
		})
	}

	if err := os.WriteFile(outputPath, []byte(fullText.String()), 0644); err != nil {
		fmt.Printf("❌ 生成失败: %v", err)
	} else {
		fmt.Printf("✔ 转换成功: %s\n", outputPath)
	}
}

func txtToepub(inputPath, outputPath, title, author, chapterReg, coverPath, cssPath, llm string, wait int) {

	// 1. 自动检测并读取内容
	contentBytes, err := os.ReadFile(inputPath)
	if err != nil {
		fmt.Printf("❌ 文件读取失败: %v", err)
		return
	}

	decodedContent := autoDecode(contentBytes)

	// 2. 初始化 EPUB
	e, _ := epub.NewEpub(title)
	e.SetAuthor(author)

	// 3. 封面与样式
	if coverPath != "" {
		internalCoverPath, _ := e.AddImage(coverPath, "cover.jpg")
		e.SetCover(internalCoverPath, "")
	}

	var internalCssPath string
	if cssPath != "" {
		internalCssPath, _ = e.AddCSS(cssPath, "style.css")
	} else {
		css := `
		body { font-family: sans-serif; line-height: 1.5; padding: 5%; color: #333; }
		h2 { text-align: center; color: #1a5276; margin: 1em 0; }
		p { text-indent: 2em; margin: 0.5em 0; color: #2c3e50;}
		span { color: #0e6251 !important; font-style: normal; letter-spacing: 0.05em; }
		pre { overflow-x: auto; background-color: #f8f8f8; padding: 10px; border-radius: 4px; font-size: 0.85em; line-height: 1.4; font-family: "Courier New", monospace; margin: 1em 0; white-space: pre; word-wrap: normal; }
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
		// 确保程序结束时删除临时文件
		defer os.Remove(tempCss.Name())

		tempCss.WriteString(css)
		tempCss.Close()

		// 4. 【关键步骤】将临时文件路径传给 AddCSS
		// 第二个参数是 EPUB 内部的目标路径名
		internalCssPath, _ = e.AddCSS(tempCss.Name(), "style.css")
	}

	// 4. 解析章节与图片
	buildEpub(e, decodedContent, chapterReg, internalCssPath, llm, wait)

	// 5. 保存
	if err := e.Write(outputPath); err != nil {
		fmt.Printf("❌ 生成失败: %v", err)
	} else {
		fmt.Printf("✔ 转换成功: %s\n", outputPath)
	}
}

func lockRegex(lines []string, customRe string) []string {
	// 1. 定义内置正则库
	patterns := []string{
		// 1. 标准强特征型 (第x章)
		`^第\s*[0-9一二三四五六七八九十百千万亿两零壹贰叁肆伍陆柒捌玖拾佰仟萬]{1,9}\s*[章节節回迴卷折篇幕序番部季集段层層场場话話页頁记記说說志考述引曲]\s*.{0,30}$`,

		// 2. 标准弱特征型 (十二章)
		`^\s*[0-9一二三四五六七八九十百千万亿两零壹贰叁肆伍陆柒捌玖拾佰仟萬]{1,9}\s*[章节節回迴卷折篇幕序番部季集段层層场場话話页頁记記说說志考述引曲]\s*.{0,30}$`,

		// 3. 符号包裹型 (【第一章】)
		`^[\[《〈〈【『「〔（<({].{0,10}[章节節回迴卷折篇幕序番部季集段层層场場话話页頁记記说說志考述引曲].{0,10}[\]》〉〉〕」』】）>)}].{0,30}$`,

		// 4. 无符号数字型(天界篇)
		`^\S{0,10}[章节節回迴卷折篇幕序番部季集段层層场場话話页頁记記说說志考述引曲]$`,

		// 5. 数字分隔型(二十一 标题)
		`^\s*[0-9一二三四五六七八九十百千万亿两零壹贰叁肆伍陆柒捌玖拾佰仟萬]{1,9}[、.：:|｜——\s-]+.{0,30}$`,

		// 6. 符号包裹数字型([01] 标题)
		`^\s*[\[《〈〈【『「〔（<({][0-9一二三四五六七八九十百千万亿两零壹贰叁肆伍陆柒捌玖拾佰仟萬]{1,9}[\]》〉〉〕」』】）>)}][、.：:|｜——\s-]?.{0,30}$`,

		// 4. 英文标题型(Chapter 1)
		`(?i)^(Chapter|Section|Case|Episode|Lesson|Clause|Article|Book|Part|Unit|Stanza|Canto|Vol|Volume|Catalog|Preface|Foreword|Prologue|Abstract|Summary|Synopsis|Opening|Ending|Afterword|Epilogue|Interlude|Appendix|Acknowledgments|Postscript|Extra|Toc|Table of Contents|Related Information|Back Matter|Final Words| Closing Remarks|Side Story)\s*[0-9]{1,5}.{0,30}$`,

		// 7. 罗马数字型
		`(?i)^(M{1,4}|M{0,4}(?:CM|CD|D?C{1,3})|M{0,4}(?:D?C{0,3})(?:XC|XL|L?X{1,3})|M{0,4}(?:D?C{0,3})(?:L?X{0,3})(?:IX|IV|V?I{1,3}))([、.：:|｜——\s-].{1,30})$`,
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
		if unicode.IsLetter(r) && !strings.Contains("一二三四五六七八九十百千万亿两零壹贰叁肆伍陆柒捌玖拾佰仟萬", string(r)) {
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
	if start < end {
		return strings.Join(lines[start+1:end], "\n")
	} else {
		return ""
	}
}

// 自动识别编码并转换为 UTF-8
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
		decoder = simplifiedchinese.GB18030.NewDecoder() // GB18030 是 GBK 的超集，兼容性更好
	case "BIG5":
		decoder = traditionalchinese.Big5.NewDecoder()
	case "UTF8":
		return string(data)
	default:
		// 如果是 ISO-8859 或其他，尝试强转 GB18030（国内小说最常见情况）
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

func buildEpub(e *epub.Epub, content, chapterReg, cssPath, llm string, wait int) {
	// dialogueReg := regexp.MustCompile(`([“"‘'].+?[”"’'])`)
	dialogueReg := regexp.MustCompile(`([“"‘'「『《〈〈【〔（<({\[].+?[”"’」』》〉〉〕】）>)}\]])`)

	// 按行分割并清理空白
	lines := strings.Split(content, "\n")

	// 预取3000行进行章节正则锁定，避免后续章节识别混乱（尤其是正文中有类似章节格式的行）
	chapterRegs := lockRegex(lines[:5000], chapterReg)

	var currentBody strings.Builder
	title := "前言"
	currentBody.WriteString(fmt.Sprintf("<h2 class='chapter-title'>%s</h2>\n", title))

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
					codeLanguage = "text" // 默认语言
				}
			} else {
				// 代码块结束，渲染并写入 body
				highlighted := highlightCode(codeBuffer.String(), codeLanguage)
				currentBody.WriteString(fmt.Sprintf("<div class='code-wrapper'>%s</div>", highlighted))
				codeBuffer.Reset()
				inCodeBlock = false
			}
			continue
		}

		if inCodeBlock {
			codeBuffer.WriteString(line + "\n")
		} else {
			// 增强：限制章节行长度，通常章节标题不会超过 50 个字符
			if isChapter(line, chapterRegs) {
				if currentBody.Len() > 0 {
					e.AddSection(wrap(currentBody.String()), title, "", cssPath)
					currentBody.Reset()
				}
				title = line
				// fmt.Printf("📖 识别到章节: %s\n", title)
				if trueTitle(title) == "" && llm != "" {
					content := strings.TrimSpace(extractChapterContent(lines[idx:idx+1000], chapterRegs))
					title = fmt.Sprintf("%s %s", title, getAiTitle(llm, content))
					time.Sleep(time.Duration(wait) * time.Millisecond)
				}
				title = strings.TrimSpace(title)
				fmt.Printf("§ 识别到章节: %s\n", title)
				currentBody.WriteString(fmt.Sprintf("<h2 class='chapter-title'>%s</h2>\n", title))
			} else {
				// 对话和其他内容处理
				processedLine := dialogueReg.ReplaceAllString(line, `<span class="dialogue">$1</span>`)
				currentBody.WriteString(fmt.Sprintf("<p class='text-para'>%s</p>\n", processedLine))
			}
		}
	}
	// 写入最后一章
	e.AddSection(wrap(currentBody.String()), title, "", cssPath)
}

func wrap(body string) string {
	return fmt.Sprintf("<body>%s</body>", body)
}

func getAiTitle(llm, chapterContent string) string {
	chapterContent = strings.TrimSpace(chapterContent)
	if chapterContent == "" {
		return ""
	}

	var llmurl, model, apikey string
	llminfo := strings.Split(llm, ":")
	if len(llminfo) < 2 {
		return ""
	}

	llmname := strings.ToLower(llminfo[0])
	apikey = llminfo[1]
	if llmname == "" || apikey == "" {
		return ""
	}

	switch {
	case strings.HasPrefix(llmname, "glm"):
		llmurl = "https://open.bigmodel.cn/api/paas/v4/chat/completions"
		model = "glm-4-flash"
		if len(strings.Split(llmname, "/")) > 1 {
			model = strings.Split(llmname, "/")[1]
		}
	case strings.HasPrefix(llmname, "gemini"):
		model = "gemini-flash-lite-latest"
		if len(strings.Split(llmname, "/")) > 1 {
			model = strings.Split(llmname, "/")[1]
		}
	default:
		return ""
	}

	payload := map[string]interface{}{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": "你是一位优秀的网文编辑，请根据正文生成一个2-7字的简炼的符合正文风格的章节标题(不要包含标点符号)，只返回标题文本。"},
			{"role": "user", "content": "内容如下：\n" + chapterContent},
		},
	}

	body, _ := json.Marshal(payload)

	// 2. 发送请求
	req, _ := http.NewRequest("POST", llmurl, bytes.NewBuffer(body))
	req.Header.Set("Authorization", "Bearer "+apikey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Printf("❌ 请求大模型 %s 失败: %v\n", llmname, err)
		return ""
	}
	defer resp.Body.Close()

	// 3. 解析响应 (直接解析到匿名 map)
	respData, _ := io.ReadAll(resp.Body)
	var result map[string]interface{}
	if err := json.Unmarshal(respData, &result); err != nil {
		fmt.Printf("❌ 解析大模型 %s 响应失败: %v\n", llmname, err)
		return ""
	}

	// 4. 通过键值路径提取数据 (断言处理)
	if choices, ok := result["choices"].([]interface{}); ok && len(choices) > 0 {
		firstChoice := choices[0].(map[string]interface{})
		message := firstChoice["message"].(map[string]interface{})
		return strings.TrimSpace(message["content"].(string))
	} else {
		fmt.Printf("❌ 大模型 %s 响应异常: %v\n", llmname, string(respData))
		return ""
	}
}
