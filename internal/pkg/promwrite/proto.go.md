# `proto.go` 技术实现文档

> 源文件：`internal/pkg/promwrite/proto.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/promwrite`

## 1. 概述

本文件是 Prometheus remote_write `WriteRequest` 的手写 proto3 编码器。仅实现 encode 路径（manager 只 push），用 ~130 行代码替代 `github.com/prometheus/prometheus/prompb` 的整条依赖链。所有字段编号与官方 Prometheus proto 一致。

## 2. 包信息

- **包名**：`promwrite`
- **所属模块**：`internal/pkg/`
- **依赖方向**：被同包 `client.go` 调用；仅依赖标准库 `encoding/binary`、`math`

## 3. 关键类型与接口

```go
const (
    wireVarint = 0 // varint
    wireI64    = 1 // 64-bit fixed (double, fixed64)
    wireBytes  = 2 // length-delimited (string, bytes, embedded message)
)

// 内部使用的 per-series sample 形态
type sampleEntry struct {
    value float64
    tsMs  int64
}
```

## 4. 关键函数与流程

### `appendVarint / appendTag / appendString / appendBytesField / appendDouble / appendInt64`
- **签名**：`func appendXxx(b []byte, fieldNum int, v T) []byte`
- **职责**：proto3 wire-format 的原子构造块
- **流程**：
  - `appendVarint`：base-128 编码，每字节高位 0x80 标记"还有后续"
  - `appendTag`：`(fieldNum << 3) | wireType` 作为 varint 写入
  - `appendString`：空字符串跳过（proto3 默认值），非空则 tag + length + bytes
  - `appendDouble`：始终写出（即使 0.0，保证 round-trip）；用 `binary.LittleEndian.PutUint64` 写 IEEE-754 位
  - `appendInt64`：0 跳过；非 0 走 varint（int64 负数会变成 10 字节，但 tsMs 不会负）
- **错误处理**：均不返回 error（编码不会失败）

### `encodeLabel / encodeSample / encodeTimeSeries / encodeWriteRequest`
- **签名**：`func encodeXxx(...) []byte`
- **职责**：组合原子函数生成各层 message
- **流程**：
  - `encodeLabel(name, value)`：field 1 = name, field 2 = value（均 string）
  - `encodeSample(value, tsMs)`：field 1 = value (double), field 2 = timestamp (varint int64)
  - `encodeTimeSeries(labels, samples)`：循环 labels 写 field 1（bytes field 嵌入 encodeLabel 输出），循环 samples 写 field 2
  - `encodeWriteRequest(seriesPayloads)`：循环每个 TimeSeries 字节切片写 field 1
- **错误处理**：无错误返回

## 5. 依赖关系

- **内部包**：无
- **外部库**：无（仅标准库）
- **被调用方**：`client.go` 的 `Write` 方法

## 6. 并发与资源管理

无并发控制。所有函数都是纯函数，对入参切片做 append 后返回新切片，无共享状态。

## 7. 设计模式与亮点

- **零依赖 protobuf 编码**：用最少代码覆盖一个稳定的小 schema，规避大型上游依赖
- **proto3 默认值优化**：空字符串 / 0 int64 跳过，减少 wire 大小；唯独 double 0 仍写出（注释说明这是为 round-trip 安全，Prom 容忍任一形态）
- **字段编号与官方对齐**：注释明示 "All field numbers below match the official Prometheus proto"，便于 wire 抓包比对
- **分层组合**：每个 message 一个 encode 函数，避免巨型函数

## 8. 注意事项

- **不支持 decode**：若未来需要读 remote_read 响应需补 decode（更复杂：需处理 packed repeated、未知字段 skip）
- **int64 负数**：`appendInt64` 用 `uint64(v)` 直接转，负数会变成 10 字节；Prom 的 timestamp 不会为负，可接受
- **double 0 的取舍**：注释承认这与 proto3 "skip default value" 习惯不同，是有意为之；如果未来 Prom 收紧校验需重新评估
- **append 性能**：每次 append 可能触发切片扩容；对极大批量场景可考虑预分配，但当前 1 sample/series 场景不构成瓶颈
