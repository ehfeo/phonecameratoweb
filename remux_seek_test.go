package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// walkOffsets 用与生产相同的健壮逻辑遍历 Segment 顶层子元素，返回 (首个Cues偏移, 首个Cluster偏移)
func walkOffsets(seg []byte) (cuesOff, clusterOff int) {
	cuesOff, clusterOff = -1, -1
	pos := 0
	for pos < len(seg) {
		id := elemID(seg, pos)
		if len(id) == 0 {
			break
		}
		p := pos + len(id)
		if p >= len(seg) {
			break
		}
		val, hl, unk, ok := readVintSize(seg, p)
		if !ok {
			break
		}
		ds := p + hl
		de := 0
		if unk {
			de = walkVariable(seg, ds, len(seg))
		} else if int(val) <= len(seg)-ds {
			de = ds + int(val)
		} else {
			break
		}
		if de < ds {
			break
		}
		switch {
		case bytes.Equal(id, []byte(cuesID)) && cuesOff < 0:
			cuesOff = pos
		case bytes.Equal(id, []byte(clusterID)) && clusterOff < 0:
			clusterOff = pos
		}
		pos = de
	}
	return cuesOff, clusterOff
}

func TestRemuxSeek(t *testing.T) {
	matches, _ := filepath.Glob("recordings/*.webm")
	if len(matches) == 0 {
		t.Skip("没有可用的 webm 样本")
	}
	for _, pth := range matches {
		b, err := os.ReadFile(pth)
		if err != nil {
			t.Fatalf("read %s: %v", pth, err)
		}
		out, err := remuxWebMBytes(b)
		if err != nil {
			t.Errorf("[%s] remux 失败: %v", pth, err)
			continue
		}
		// 1) EBML 头仍在
		if len(out) < 4 || !bytes.Equal(out[0:4], []byte(ebmlHeaderID)) {
			t.Errorf("[%s] EBML 头丢失", pth)
			continue
		}
		// 2) 输出文件里的 Segment 应为有限大小(能被 elemBounds 解析)，且大小与内容一致
		_, _, ebmlEnd, ok0 := elemBounds(out, 0)
		if !ok0 {
			t.Errorf("[%s] 输出 EBML 头解析失败", pth)
			continue
		}
		if string(out[ebmlEnd:ebmlEnd+4]) != segmentID {
			t.Errorf("[%s] 输出缺 Segment", pth)
			continue
		}
		szHlen := vintLen(out[ebmlEnd+4])
		segDS := ebmlEnd + 4 + szHlen
		seg := out[segDS:]

		// 3) Cues 必须出现在首个 Cluster 之前
		oc, okc := walkOffsets(seg)
		if okc < 0 {
			t.Errorf("[%s] 输出中未找到 Cluster", pth)
			continue
		}
		if oc < 0 {
			t.Errorf("[%s] 输出中没有 Cues", pth)
			continue
		}
		if oc >= okc {
			t.Errorf("[%s] Cues 未前移: cues@%d cluster@%d", pth, oc, okc)
			continue
		}
		// 4) 解码新 Cues，核对每个 CuePoint 的 CueClusterPosition 确实落在某个 Cluster 起始、
		//    CueRelativePosition 落在 SimpleBlock(0xA3) 上。
		if err := verifyCues(seg, oc, len(b), t, pth); err != nil {
			t.Errorf("[%s] Cues 内容非法: %v", pth, err)
			continue
		}
		t.Logf("OK %s: %d->%d bytes, cues@%d < firstCluster@%d (Cues 内容已核对)", pth, len(b), len(out), oc, okc)
	}
}

func verifyCues(seg []byte, cuesOff int, _ int, t *testing.T, pth string) error {
	// 解析 Cues 元素
	_, cDS, cDE, ok := elemBounds(seg, cuesOff)
	if !ok {
		return fmtErr("Cues 无法解析")
	}
	pos := cDS
	n := 0
	for pos < cDE {
		id := elemID(seg, pos)
		_, ds, de, ok2 := elemBounds(seg, pos)
		if !ok2 {
			break
		}
		if bytes.Equal(id, []byte(cuePointID)) {
				n++
				var cp, cr int64
				cp, cr = -1, -1
				// 下钻一层找 CueTrackPositions
				pn := ds
				for pn < de {
					subID := elemID(seg, pn)
					if len(subID) == 0 {
						break
					}
					_, subDS, subDE, ok3 := elemBounds(seg, pn)
					if !ok3 {
						break
					}
					if bytes.Equal(subID, []byte(cueTrackPosID)) {
						// CueTrackPositions 内部含 CueClusterPosition/CueRelativePosition
						dn := subDS
						for dn < subDE {
							outID := elemID(seg, dn)
							if len(outID) == 0 {
								break
							}
							_, oDS, oDE, ok3b := elemBounds(seg, dn)
							if !ok3b {
								break
							}
							if bytes.Equal(outID, []byte(cueClusterPosID)) && oDS < oDE {
								cp = int64(readUint(seg[oDS:oDE]))
							}
							if bytes.Equal(outID, []byte(cueRelPosID)) && oDS < oDE {
								cr = int64(readUint(seg[oDS:oDE]))
							}
							dn = oDE
						}
					}
					pn = subDE
				}
			if cp < 0 {
				return fmtErr("CuePoint#%d 缺 CueClusterPosition", n)
			}
			if int(cp) >= len(seg) {
				return fmtErr("CueClusterPosition %d 越界", cp)
			}
			if !bytes.Equal(seg[cp:cp+4], []byte(clusterID)) {
				return fmtErr("CueClusterPosition %d 不是 Cluster 起始", cp)
			}
			if cr < 0 {
				return fmtErr("CuePoint#%d 缺 CueRelativePosition", n)
			}
			blk := cp + cr
			if int(blk) >= len(seg) {
				return fmtErr("Cue 指向越界")
			}
			if !bytes.Equal(seg[blk:blk+1], []byte(simpleBlockID)) && !bytes.Equal(seg[blk:blk+1], []byte(blockGroupID)) {
				return fmtErr("Cue 指向的第%d个块不是 SimpleBlock/BlockGroup", n)
			}
		}
		pos = de
	}
	if n == 0 {
		return fmtErr("Cues 为空")
	}
	_ = t
	return nil
}

type famErr struct{ s string }
func (e famErr) Error() string { return e.s }
func fmtErr(f string, a ...interface{}) error {
	return famErr{fmt.Sprintf(f, a...)}
}