package main

// 兜底导出：当剪映草稿结构不可用时，导出「带时间码的文案 + CSV + 素材清单」，
// 打包成一个 zip 下载，供手工对齐或交给第三方工具使用。

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

func tc(sec float64) string {
	if sec < 0 || sec != sec {
		sec = 0
	}
	m := int(sec / 60)
	s := sec - float64(m*60)
	return fmt.Sprintf("%02d:%05.2f", m, s)
}

type exportRow struct {
	No        string
	Content   string
	Narration string
	Copy      string
	File      string
	Src       string
	In        float64
	Out       float64
	HasTime   bool
	Start     float64
	CumKnown  bool
}

func (a *App) collectExportRows(doc Doc) []exportRow {
	rows := make([]exportRow, 0, len(doc.Shots))
	cum := 0.0
	cumKnown := true
	for _, shot := range doc.Shots {
		r := exportRow{
			No:        shot.No,
			Content:   shot.Content,
			Narration: shot.Narration,
			Copy:      shot.Copy,
			Start:     cum,
			CumKnown:  cumKnown,
		}
		if shot.Video != nil && shot.Video.File != "" {
			r.File = shot.Video.File
			r.Src = shot.Video.Src
			if shot.Video.Out > shot.Video.In {
				r.In = shot.Video.In
				r.Out = shot.Video.Out
				r.HasTime = true
				cum += shot.Video.Out - shot.Video.In
			} else {
				cumKnown = false
			}
		} else {
			cumKnown = false
		}
		rows = append(rows, r)
	}
	return rows
}

func csvBytes(header []string, rows [][]string) []byte {
	var buf bytes.Buffer
	buf.WriteString("\ufeff") // Excel 打开中文 CSV 不乱码
	w := csv.NewWriter(&buf)
	_ = w.Write(header)
	for _, r := range rows {
		_ = w.Write(r)
	}
	w.Flush()
	return buf.Bytes()
}

func (a *App) handleExportFallback(w http.ResponseWriter, r *http.Request) {
	product := r.URL.Query().Get("product")
	script := r.URL.Query().Get("script")
	dir, err := a.scriptDir(product, script, true)
	if err != nil {
		failStage(w, http.StatusNotFound, "读取", err.Error())
		return
	}
	var doc Doc
	if room := a.existingRoom(dir); room != nil {
		doc = room.docNow()
	} else {
		doc, err = readDoc(filepath.Join(dir, docFileName), product, script)
		if err != nil {
			failStage(w, http.StatusInternalServerError, "读取", err.Error())
			return
		}
	}
	rows := a.collectExportRows(doc)

	var timecode, readme strings.Builder
	fmt.Fprintf(&timecode, "%s / %s\n导出时间：%s\n\n", product, script, time.Now().Format("2006-01-02 15:04:05"))
	for _, row := range rows {
		span := "—"
		if row.HasTime {
			span = tc(row.In) + " - " + tc(row.Out) + "（时长 " + tc(row.Out-row.In) + "）"
		}
		start := "—"
		if row.CumKnown {
			start = tc(row.Start)
		}
		fmt.Fprintf(&timecode, "镜头 %s  [%s]  累计起点 %s\n", row.No, span, start)
		if strings.TrimSpace(row.Content) != "" {
			fmt.Fprintf(&timecode, "  画面：%s\n", row.Content)
		}
		if strings.TrimSpace(row.Narration) != "" {
			fmt.Fprintf(&timecode, "  口播：%s\n", row.Narration)
		}
		if strings.TrimSpace(row.Copy) != "" {
			fmt.Fprintf(&timecode, "  文案：%s\n", row.Copy)
		}
		if row.File != "" {
			fmt.Fprintf(&timecode, "  素材：%s\n", row.File)
		}
		timecode.WriteString("\n")
	}

	shotRows := make([][]string, 0, len(rows))
	materialSeen := map[string]bool{}
	var materialRows [][]string
	for _, row := range rows {
		in, out, dur, start := "—", "—", "—", "—"
		if row.HasTime {
			in, out, dur = tc(row.In), tc(row.Out), tc(row.Out-row.In)
		}
		if row.CumKnown {
			start = tc(row.Start)
		}
		shotRows = append(shotRows, []string{row.No, row.Content, row.Narration, row.Copy, row.File, in, out, dur, start})
		if row.File != "" && !materialSeen[row.File] {
			materialSeen[row.File] = true
			src := "脚本目录"
			if row.Src == "temp" {
				src = "剪辑素材（_剪辑素材）"
			}
			materialRows = append(materialRows, []string{row.File, src, row.No})
		}
	}

	readme.WriteString("剪映导入说明\n\n")
	readme.WriteString("1. 剪映专业版没有公开的「导入工程」接口，draft_content.json 的字段随版本变化，\n")
	readme.WriteString("   直接生成的草稿存在打不开的风险，所以这里导出的是通用素材。\n\n")
	readme.WriteString("2. 用法：新建剪映草稿，把素材按「镜头文案.csv」的顺序拖到时间线，\n")
	readme.WriteString("   用「累计起点」对齐；「文案-时间码.txt」用于字幕 / 配音对时。\n\n")
	readme.WriteString("3. 素材文件不在压缩包里（体积太大），请从脚本目录或 _剪辑素材 目录另行拷贝。\n\n")
	readme.WriteString("4. 剪映草稿默认目录（Windows）：\n")
	readme.WriteString("   C:\\Users\\<用户名>\\AppData\\Local\\JianyingPro\\User Data\\Projects\\com.lveditor.draft\\\n")
	readme.WriteString("   macOS：~/Movies/JianyingPro/User Data/Projects/com.lveditor.draft/\n")
	readme.WriteString("   本导出不会写入上述目录。\n")

	name := sanitizeDraftName(product + "-" + script)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", "attachment; filename=\"export.zip\"; filename*=UTF-8''"+url.PathEscape(name+".zip"))
	zw := zip.NewWriter(w)
	add := func(n string, data []byte) {
		f, err := zw.Create(n)
		if err != nil {
			return
		}
		_, _ = f.Write(data)
	}
	add("文案-时间码.txt", []byte(timecode.String()))
	add("镜头文案.csv", csvBytes(
		[]string{"序号", "镜头内容", "口播", "文案", "素材文件", "入点", "出点", "时长", "累计起点"},
		shotRows))
	add("素材清单.csv", csvBytes([]string{"素材文件", "来源", "使用镜头"}, materialRows))
	add("导入说明.txt", []byte(readme.String()))
	_ = zw.Close()
}
