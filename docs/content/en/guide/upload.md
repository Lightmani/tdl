---
title: "Upload"
weight: 40
---

# Upload

## Upload Files

Upload specified files and directories to `Saved Messages`:

{{< command >}}
tdl up -p /path/to/file -p /path/to/dir
{{< /command >}}

## Custom Destination

Upload to custom chat.

{{< include "snippets/chat.md" >}}

### Specific Chat

Upload to specific one chat:

{{< command >}}
tdl up -p /path/to/file -c CHAT
{{< /command >}}

Upload to specific topic in a forum chat:

{{< command >}}
tdl up -p /path/to/file -c CHAT --topic TOPIC_ID
{{< /command >}}

### Message Routing

Upload to different chats by message router which is based on [expression](/reference/expr).

{{< hint warning >}}
The `--to` flag is conflicted with the `-c/--chat` and `--topic` flags. You can only use one of them.
{{< /hint >}}

List all available fields:

{{< command >}}
tdl up -p /path/to/file --to -
{{< /command >}}

Upload to `CHAT1` if MIME contains `video`, otherwise upload to `Saved Messages`:

{{< hint info >}}
You must return a **string** or **struct** as the target CHAT, and empty string means upload to `Saved Messages`.
{{< /hint >}}

{{< command >}}
tdl up -p /path/to/file \
--to 'MIME contains "video" ? "CHAT1" : ""'
{{< /command >}}

Upload to `CHAT1` if MIME contains `video`, otherwise upload to reply to message/topic `4` in `CHAT2`:

{{< command >}}
tdl up -p /path/to/file \
--to 'MIME contains "video" ? "CHAT1" : { Peer: "CHAT2", Thread: 4 }'
{{< /command >}}

Pass a file name if the expression is complex:

{{< details "router.txt" >}}
Write your expression like `switch`:

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

## Custom Parameters

Upload with 8 threads per task, 4 concurrent tasks:

{{< command >}}
tdl up -p /path/to/file -t 8 -l 4
{{< /command >}}

## Custom Caption

Custom caption is based on [expression](/reference/expr). The default caption is the file name with extension (`FileName + FileExt`).

List all available fields:

{{< command >}}
tdl up -p /path/to/file --caption -
{{< /command >}}

Custom simple caption:
{{< command >}}
tdl up -p /path/to/file --caption 'FileName + " - uploaded by tdl"'
{{< /command >}}

File name without extension:
{{< command >}}
tdl up -p /path/to/file --caption 'FileName'
{{< /command >}}

No caption:
{{< command >}}
tdl up -p /path/to/file --caption '""'
{{< /command >}}

Write styled message with [HTML](https://core.telegram.org/bots/api#html-style):
{{< command >}}
tdl up -p /path/to/file --caption  \
'FileName + `<b>Bold</b> <a href="https://example.com">Link</a>`'
{{< /command >}}

Pass a file name if the expression is complex:

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

## Filters

Upload files with extension filters:

{{< hint warning >}}
The extension is only matched with the file name, not the MIME type. So it may not work as expected.

Whitelist and blacklist can not be used at the same time.
{{< /hint >}}

Whitelist: Only upload files with `.jpg` `.png` extension

{{< command >}}
tdl up -p /path/to/file -p /path/to/dir -i jpg,png
{{< /command >}}

Blacklist: Upload all files except `.mp4` `.flv` extension

{{< command >}}
tdl up -p /path/to/file -p /path/to/dir -e mp4 -e flv
{{< /command >}}

## Resume/Restart

A failed file doesn't stop the others. When the upload finishes, failed files are listed with their errors,
and successfully uploaded files are recorded (immediately after each upload, so the record also survives a crash or `Ctrl+C`).

Run the same command again to upload only the failed and remaining files. tdl asks whether to skip the uploaded files.
Changed files (size or modification time) are always uploaded again.

Resume without UI interaction:

{{< command >}}
tdl up -p /path/to/dir -c CHAT --continue
{{< /command >}}

Restart (upload all files again) without UI interaction:

{{< command >}}
tdl up -p /path/to/dir -c CHAT --restart
{{< /command >}}

{{< hint info >}}
An upload is identified by its `-p` paths and destination (`-c`, `--topic`, `--to`), so adding new files to the directory doesn't break resuming.
The record is deleted once all files are uploaded, so running the same command after a complete upload uploads all files again.
{{< /hint >}}

## Delete Local

Delete the uploaded file after uploading successfully:

{{< command >}}
tdl up -p /path/to/file --rm
{{< /command >}}

## Photo

Upload images as photos instead of documents:

{{< command >}}
tdl up -p /path/to/file --photo
{{< /command >}}

## Video

Videos in MP4/MOV/M4V/3GP and MKV/WebM containers are uploaded as videos which can be previewed and played in Telegram,
whatever their codec is (H.264, H.265/HEVC, AV1, VP9...). Their duration and resolution (rotation applied) are read from the file.
Other containers (e.g. AVI, FLV) or broken files are uploaded as documents.

To set a thumbnail for `video.mp4`, put a JPEG image named `video.thumb` next to it (Telegram requires it to be smaller than 200 KB and not larger than 320x320).
