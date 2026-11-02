---
title: "上传"
weight: 40
---

# 上传

## 上传文件

上传指定的文件和目录到 `保存的消息`：

{{< command >}}
tdl up -p /path/to/file -p /path/to/dir
{{< /command >}}

## 自定义目标

上传到自定义聊天。

{{< include "snippets/chat.md" >}}

## 指定聊天

上传到指定的聊天：

{{< command >}}
tdl up -p /path/to/file -c CHAT
{{< /command >}}

上传到论坛型聊天的指定主题：

{{< command >}}
tdl up -p /path/to/file -c CHAT --topic TOPIC_ID
{{< /command >}}

## 消息路由

通过基于[表达式](/reference/expr)的消息路由，将文件上传到不同的聊天：

{{< hint warning >}}
`--to` 标志与 `-c/--chat` 和 `--topic` 标志冲突，只能使用其中一个。
{{< /hint >}}

列出所有可用字段：

{{< command >}}
tdl up -p /path/to/file --to -
{{< /command >}}

如果 MIME 包含 `video` 则上传到 `CHAT1`，否则上传到 `收藏夹`：

{{< hint info >}}
必须返回一个字符串或结构体作为目标聊天，空字符串表示上传到 `收藏夹`。
{{< /hint >}}

{{< command >}}
tdl up -p /path/to/file \
--to 'MIME contains "video" ? "CHAT1" : ""'
{{< /command >}}

如果 MIME 包含 `video` 则上传到 `CHAT1`，否则回复 `CHAT2` 的消息/主题 `4`：

{{< command >}}
tdl up -p /path/to/file \
--to 'MIME contains "video" ? "CHAT1" : { Peer: "CHAT2", Thread: 4 }'
{{< /command >}}

如果表达式较复杂，可以传递文件名：

{{< details "router.txt" >}}
像使用 `switch` 一样编写表达式：

```javascript
MIME contains "video" ? "CHAT1" :
FileExt contains ".mp3" ? "CHAT2" :
FileName contains "chat3" > 30 ? {Peer: "CHAT3", Thread: 101} :
""
```

{{< /details >}}

{{< command >}}
tdl up -p /path/to/file --to router.txt
{{< /command >}}

## 自定义参数

使用每个任务8个线程、4个并发任务上传：

{{< command >}}
tdl up -p /path/to/file -t 8 -l 4
{{< /command >}}

## 自定义标题

使用[表达式引擎](/reference/expr)编写自定义标题。默认标题为带扩展名的文件名（`FileName + FileExt`）。

列出所有可用字段：

{{< command >}}
tdl up -p /path/to/file --caption -
{{< /command >}}

自定义简单的标题：
{{< command >}}
tdl up -p ./path/to/file --caption 'FileName + " - uploaded by tdl"'
{{< /command >}}

不带扩展名的文件名：
{{< command >}}
tdl up -p /path/to/file --caption 'FileName'
{{< /command >}}

不添加标题：
{{< command >}}
tdl up -p /path/to/file --caption '""'
{{< /command >}}

以[HTML](https://core.telegram.org/bots/api#html-style)格式编写带有样式的消息：
{{< command >}}
tdl up -p /path/to/file --caption  \
'FileName + `<b>Bold</b> <a href="https://example.com">Link</a>`'
{{< /command >}}

如果表达式较复杂，可以传递文件名：

{{< details "caption.txt" >}}

```javascript
repeat(FileName, 2) + `
<a href="https://www.google.com">Google</a>
<a href="https://www.bing.com">Bing</a>
<b>bold</b>
<i>italic</i>
<code>code</code>
<tg-spoiler>spoiler</tg-spoiler>
<pre><code class="language-go">
package main

import "fmt"

func main() {
    fmt.Println("hello world")
}
</code></pre>
` + MIME
```

{{< /details >}}

{{< command >}}
tdl up -p /path/to/file --caption caption.txt
{{< /command >}}

## 过滤器

使用扩展名过滤器上传文件：

{{< hint warning >}}
扩展名仅与文件名匹配，而不是 MIME 类型。因此，这可能不会按预期工作。

白名单和黑名单不能同时使用。
{{< /hint >}}

白名单：只上传扩展名为 `.jpg` `.png` 的文件

{{< command >}}
tdl up -p /path/to/file -p /path/to/dir -i jpg,png
{{< /command >}}

黑名单：上传除了扩展名为 `.mp4` `.flv` 的所有文件

{{< command >}}
tdl up -p /path/to/file -p /path/to/dir -e mp4 -e flv
{{< /command >}}

## 断点续传/重新开始

单个文件上传失败不会中断其他文件。上传结束时会列出所有失败的文件及原因，
已成功上传的文件会被记录（每个文件上传成功后立即记录，因此程序崩溃或按下 `Ctrl+C` 后也不会丢失）。

再次运行相同的命令即可只上传失败和剩余的文件，tdl 会询问是否跳过已上传的文件。
内容发生变化（大小或修改时间改变）的文件总是会被重新上传。

在不需要交互的情况下继续上传：

{{< command >}}
tdl up -p /path/to/dir -c CHAT --continue
{{< /command >}}

在不需要交互的情况下重新开始（重新上传所有文件）：

{{< command >}}
tdl up -p /path/to/dir -c CHAT --restart
{{< /command >}}

{{< hint info >}}
一次上传由 `-p` 路径和目标（`-c`、`--topic`、`--to`）确定，因此向目录中添加新文件不会影响续传。
所有文件都上传成功后记录会被删除，因此完整上传后再次运行相同命令会重新上传所有文件。
{{< /hint >}}

## 自动删除

删除已上传成功的文件：

{{< command >}}
tdl up -p /path/to/file --rm
{{< /command >}}

## 照片

将图像作为照片而不是文件上传：

{{< command >}}
tdl up -p /path/to/file --photo
{{< /command >}}

## 视频

MP4/MOV/M4V/3GP 和 MKV/WebM 容器的视频会作为视频上传，可以在 Telegram 中直接预览和播放，
与视频编码无关（H.264、H.265/HEVC、AV1、VP9 等均可）。时长和分辨率（已处理旋转）从文件中读取。
其他容器（如 AVI、FLV）或损坏的文件会作为文件上传。

如需为 `video.mp4` 设置缩略图，将名为 `video.thumb` 的 JPEG 图片放在同一目录下即可（Telegram 要求小于 200 KB 且宽高不超过 320）。
