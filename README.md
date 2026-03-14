# README

## 简介

iepub是一个golang编写的支持AI大模型的轻量级txt文本格式和epub电子书格式相互转换工具.

功能特点:

- 单二进制文件运行, 无任何依赖
- 获取epub文件元数据, 封面和目录, 修改元数据和封面
- 根据文件后缀名自动识别转换方向, 支持根据输入文件名自动指定输出文件名和书名
- txt转epub时可指定元数据(书名, 作者, 协作, 简介, 语言, 发行日期, 出版商等), 封面图, css样式, 章节识别正则
- 内置简单的css样式提升生成的epub电子书阅读体验(正文, 章节标题, 人物对话, 代码块)
- 内置多种章节识别正则表达式并通过预取识别将最匹配的正则作为全文的章节识别正则
- txt转epub时, 对于没有具体章节标题的章节, 支持通过AI分析章节内容自动生成有意义的章节标题
- 支持以server方式运行, 以HTTP API接口提供元数据读取, 元数据和封面修改, txt转epub和epub转txt功能

> 本工具仅针对网文这种单层章节的文本作过转换测试, 不支持生成分层章节, 不保证对其他txt文本的转换质量.

## 构建

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags netgo -o iepub main.go # linux amd64
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -tags netgo -ldflags="-s -w" -o iepub_arm64 main.go # linux arm64
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o iepub.exe main.go       # windlows amd64
```

## 使用

```bash
# 命令帮助
iepub -h

# 以server方式运行
iepub server
iepub -p 2233 server

# 获取epub元数据
iepub -i xxx.epub

# 修改epub元数据(所有参数都是可选的)
iepub -t <书名> -a <作者> -x <协作> -g <语言> -e <描述> -k <标签> -d <发行日> -u <出版商> -b <版权声明> -c <封面图> xxx.epub

# 将xxx.txt文件转换为名为xxx.epub的epub格式(未指定输出文件名时将自动设为输入文件名)
iepub xxx.txt   

# 将xxx.epub文件转换为名为xxx.txt的txt格式(未指定输出文件名时将自动设为输入文件名)
iepub xxx.epub  

# txt转epub时指定epub电子书元数据(所有参数都是可选的)
# 工具内置一套简单的css样式(body:正文, h2:章节标题, p:段落, span:对话, pre:代码块), 命令参数指定css样式将忽略内置样式
# 工具内置多套章节识别正则表达式, 并自动使用最符合的正则表达式切分章节, 命令参数指定章节识别正则表达式将忽略内置表达式
iepub -t <书名> -a <作者> -x <协作> -g <语言> -e <描述> -k <标签> -d <发行日> -u <出版商> -b <版权声明> -c <封面图> -s <css样式> -m <图片标签识别正则> -r <章节识别正则> -o <输出文件> xxx.txt

# txt转epub时使用大模型自动成章节标题(当前支持glm(智谱清言), gemini(Google-Gemini), silicon(硅基流动)的大模型API)
# 由于免费模型有频率和Token限制, 支持通过-w参数(单位毫秒)控制API调用频率
# 支持的三种大模型都内置了默认模型(智谱清言: glm-4-flash, Gemini: gemini-flash-lite-latest, 硅基流动:Qwen/Qwen3-8B等), 对于内置了多个模型的提供商(如硅基流动), 将在调用达到限制时自动切换到下一个模型
iepub -w 10000 -l "glm:<api-key>" xxx.txt  # 指定ai提供商但不指定模型(自动使用内置免费模型)
iepub -w 10000 -l "siliconflow/Qwen/Qwen3-8B:<api-key>" xxx.txt  # 指定ai提供商和模型
```
