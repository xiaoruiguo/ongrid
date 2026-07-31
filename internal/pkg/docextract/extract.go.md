# `extract.go` 技术实现文档

> 源文件：`internal/pkg/docextract/extract.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/docextract`

## 1. 概述

该文件实现知识库文件（.md / .txt / .pdf / .docx）的纯文本抽取，为 RAG pipeline（chunk → embed → upsert）提供索引原料。纯 Go 无 CGO：md/txt 直通、PDF 走 `ledongthuc/pdf`、docx 用 stdlib `archive/zip` + 轻量 XML 遍历。扫描版 / 图片 PDF 与加密文件返回空或错误——OCR 明确排除在范围外（ADR-028 phase-2）。

## 2. 包信息

- **包名**：`docextract`
- **所属模块**：`internal/pkg/`（基础设施层）
- **依赖方向**：被 manager knowledge BC 调用；依赖 `github.com/ledongthuc/pdf` + 标准库。

## 3. 关键类型与接口

无显著类型定义（仅顶层函数）。

## 4. 关键函数与流程

### `Supported`
- **签名**：`func Supported(filename string) bool`
- **职责**：判断扩展名是否受支持（`.md` / `.markdown` / `.txt` / `.text` / `.pdf` / `.docx`）。
- **流程**：`strings.ToLower(filepath.Ext(filename))` switch。

### `Extract`
- **签名**：`func Extract(filename string, data []byte) (string, error)`
- **职责**：按扩展名分发到对应抽取器。
- **流程**：
  - md / txt：校验 UTF-8 合法性，`string(data)` 直通。
  - pdf：`extractPDF`。
  - docx：`extractDOCX`。
  - 其他：返回 user-facing error。
- **错误处理**：错误信息面向用户（出现在上传响应中），措辞友好。

### `extractPDF`
- **签名**：`func extractPDF(data []byte) (string, error)`
- **流程**：
  1. `pdf.NewReader(bytes.NewReader(data), int64(len(data)))`。
  2. `r.GetPlainText()` 取文本流。
  3. `io.Copy` 到 `strings.Builder`。
  4. `TrimSpace` 后空 → 返回友好错误 `no extractable text in pdf (scanned/image PDFs need OCR, not supported)`，让运维知道是扫描件而非 bug。
- **错误处理**：每步用 `%w` 包装并加前缀 `read pdf` / `extract pdf text` / `read pdf text`。

### `extractDOCX`
- **签名**：`func extractDOCX(data []byte) (string, error)`
- **流程**：
  1. `zip.NewReader` 解压 OOXML。
  2. 遍历 zip 文件找 `word/document.xml`；缺失 → error。
  3. `xml.NewDecoder` 流式解析，状态机跟踪：
     - `<w:t>` 开始 → `inText=true`；`<w:t>` 结束 → `inText=false`。
     - `<w:p>` 结束 → 写入 `\n`（段落分隔）。
     - `<w:tab>` 结束 → 写入 `\t`。
     - `CharData` 且 `inText` → 写入 builder。
  4. `TrimSpace` 后空 → error `no extractable text in docx`。
- **设计理由**：避免重量级 OOXML 库——只需文本不需样式，stdlib XML 解码足够。
- **错误处理**：每步用 `%w` 包装并加前缀。

## 5. 依赖关系

- **内部包**：无。
- **外部库**：`github.com/ledongthuc/pdf`；标准库 `archive/zip` / `bytes` / `encoding/xml` / `io` / `path/filepath` / `strings` / `unicode/utf8`。
- **被调用方**：manager knowledge BC（文档上传 handler）。

## 6. 并发与资源管理

无并发控制。所有函数纯函数式，输入 `[]byte` 输出 `(string, error)`，无共享状态。`io.Copy` 与 `xml.Decoder` 都是流式处理，内存占用与文件大小成线性。

## 7. 设计模式与亮点

- **纯 Go 无 CGO**：PDF / DOCX 都用纯 Go 库，构建链简单，跨平台交叉编译无障碍。
- **dispatch by extension**：`Extract` 一个入口分发到各格式专用函数，调用方接口统一。
- **友好错误**：扫描 PDF / 空文档返回明确文案，告诉运维"是文件本身问题不是 bug"，降低支持成本。
- **轻量 XML 状态机**：docx 解析用最小状态机（`inText` 布尔 + EndElement 分支）提取文本，避免引入 OOXML schema 库。
- **UTF-8 校验前置**：md/txt 在转 string 前用 `utf8.Valid` 校验，避免后续 chunk / embed 阶段遇到乱码。

## 8. 注意事项

- **PDF 仅取嵌入文本层**：扫描版 / 图片 PDF 返回空错误；OCR 是 ADR-028 phase-2 范围，当前用户需自行转文本后上传。
- **加密文件**：PDF 加密 / docx 加密会失败，错误信息可能不够明确，需在上层 handler 友好提示。
- **大文件内存**：`data []byte` 入参意味着整个文件先读入内存；超大文件（>100MB）需评估流式接口。
- **docx 不解析样式**：仅提取文本，表格 / 列表 / 图片 / 公式均丢失；复杂文档检索质量可能下降。
- **PDF 表格还原差**：`GetPlainText` 不保留表格结构，表格内容会被压平为流文本，列对齐丢失。
- **扩展新格式**：新增（如 .pptx / .xlsx）需在 `Supported` 与 `Extract` 同步加分支，并实现专用 extractor；维护时易遗漏一处。
