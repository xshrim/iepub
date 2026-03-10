package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

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
}

var defaultChapterRes = []string{
	// 1. 标准强特征型 (第x章)
	`(?m)^第?\s*[0-9一二三四五六七八九十百千万亿两零壹贰叁肆伍陆柒捌玖拾佰仟萬\S]{1,10}\s*[章节節回迴卷折篇幕序番]\s*.*$`,

	// 2. 符号包裹型 (【第一章】)
	`(?m)^[\[《〈〈【『「〔（<({].*[章节節回迴卷折篇幕序番].*[\]》〉〉〕」』】）>)}].*$`,

	// 3. 英文与特殊词汇
	`(?m)(?i)^(Chapter|Section|Case|Episode|Part|Vol|Volume)\s*[0-9]{1,4}.*$`,
	`(?m)^\s*(序言|后记|後記|番外|引子|前言|自序|终章|終章|结局|結局|楔子|致谢|致謝|附录|附錄|简介|簡介|内容简介|內容簡介|作品相关|作品相關|写在最后|寫在最後).*$`,

	// 4. 数字分隔符型 (01. 标题)
	`(?m)^[\[《〈〈【『「〔（<({]?[0-9]{1,4}[\]》〉〉〕」』】）>)}]?\s*([、.：:：|｜\s-]|——)\s*.*$`,

	// 5. 纯汉字序号 (限制长度以防误触正文)
	// 匹配如 "二十一 这里的风景"
	`(?m)^\s*[一二三四五六七八九十百千万亿两零壹贰叁肆伍陆柒捌玖拾佰仟萬]{1,10}[、\s：:]+.*$`,

	// 6. 罗马数字
	`(?m)^(?i)(?=[MDCLXVI])(M{0,4}(CM|CD|D?C{0,3})(XC|XL|L?X{0,3})(IX|IV|V?I{0,3}))([.、\s：:].*)$`,
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
	flag.Parse()

	if cfg.InputPath == "" && flag.NArg() > 0 {
		cfg.InputPath = flag.Arg(0)
	}
	if cfg.InputPath == "" {
		fmt.Println("❌ 错误: 请提供输入文件")
		fmt.Println("用法示例: iepub input.txt 或 iepub -i input.txt")
		return
	}

	fileName := filepath.Base(cfg.InputPath)
	ext := filepath.Ext(fileName)
	fname := strings.TrimSuffix(fileName, ext)

	if cfg.OutputPath == "" {
		// 获取文件名并去掉后缀
		cfg.OutputPath = fname + ".epub"
		fmt.Printf("ℹ 未指定输出文件名，自动使用输入文件名: [%s]\n", cfg.OutputPath)
	}

	// 1. 自动检测并读取内容
	contentBytes, err := os.ReadFile(cfg.InputPath)
	if err != nil {
		log.Fatal(err)
	}

	decodedContent := autoDecode(contentBytes)

	if cfg.Title == "" {
		cfg.Title = fname
		fmt.Printf("ℹ 未指定书名，自动使用文件名: [%s]\n", cfg.Title)
	}
	// 2. 初始化 EPUB
	e, _ := epub.NewEpub(cfg.Title)
	e.SetAuthor(cfg.Author)

	// 3. 封面与样式
	if cfg.CoverPath != "" {
		internalCoverPath, _ := e.AddImage(cfg.CoverPath, "cover.jpg")
		e.SetCover(internalCoverPath, "")
	}

	var internalCssPath string
	if cfg.CssPath != "" {
		internalCssPath, _ = e.AddCSS(cfg.CssPath, "style.css")
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
	parseAndBuild(e, decodedContent, cfg, internalCssPath)

	// 5. 保存
	if err := e.Write(cfg.OutputPath); err != nil {
		log.Fatalf("生成失败: %v", err)
	}
	fmt.Printf("✨ 转换成功: %s\n", cfg.OutputPath)
}

func isChapter(line string, customRe *regexp.Regexp) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		return false
	}

	runeLine := []rune(line)
	if len(runeLine) > 50 || len(runeLine) < 2 {
		return false
	}
	lastChar := runeLine[len(runeLine)-1]
	if strings.ContainsRune("，。：”》", lastChar) {
		return false
	}

	// 1. 优先尝试用户自定义正则
	if customRe != nil && customRe.MatchString(line) {
		return true
	}

	// 2. 尝试预设的高兼容性正则库
	for _, reStr := range defaultChapterRes {
		matched, _ := regexp.MatchString(reStr, line)
		if matched {
			return true
		}
	}
	return false
}

// 自动识别编码并转换为 UTF-8
func autoDecode(data []byte) string {
	detector := chardet.NewTextDetector()
	result, err := detector.DetectBest(data)
	if err != nil {
		return string(data)
	}
	normalizedCharset := strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(result.Charset, "-", ""), " ", ""))

	fmt.Printf("🔍 检测到编码: %s (置信度: %d%%)\n", normalizedCharset, result.Confidence)

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

func parseAndBuild(e *epub.Epub, content string, cfg AdvancedConfig, cssPath string) {
	// 预编译用户正则
	var userRe *regexp.Regexp
	if cfg.ChapterRe != "" {
		userRe = regexp.MustCompile(cfg.ChapterRe)
	}

	// dialogueReg := regexp.MustCompile(`([“"‘'].+?[”"’'])`)
	dialogueReg := regexp.MustCompile(`([“"‘'「『《〈〈【〔（<({\[].+?[”"’」』》〉〉〕】）>)}\]])`)

	// 按行分割并清理空白
	lines := strings.Split(content, "\n")
	var currentBody strings.Builder
	title := "前言"
	currentBody.WriteString(fmt.Sprintf("<h2 class='chapter-title'>%s</h2>\n", title))

	var inCodeBlock bool
	var codeBuffer strings.Builder
	var codeLanguage string

	for _, line := range lines {
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
			if isChapter(line, userRe) {
				if currentBody.Len() > 0 {
					e.AddSection(wrap(currentBody.String()), title, "", cssPath)
					currentBody.Reset()
				}
				title = line
				fmt.Printf("📖 识别到章节: %s\n", title)
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
