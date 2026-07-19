package reportpng

import (
	"image"
	"io"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// reportFaces 是一次渲染使用的字号集合。不同区域使用独立 Face，Close 会统一释放。
type reportFaces struct {
	title    font.Face
	subtitle font.Face
	header   font.Face
	body     font.Face
	small    font.Face
	all      []font.Face
	cjk      bool
}

// newReportFaces 创建标题、表头、正文和页脚字号。DPI 固定为 72，使字号与像素尺寸
// 一一对应，输出不会受服务器显示缩放配置影响。
func newReportFaces() (*reportFaces, error) {
	regular, boldFont, preferred, err := loadFontSources()
	if err != nil {
		return nil, err
	}
	faces := &reportFaces{}
	makeFace := func(size float64, bold bool) (font.Face, error) {
		fallback := regular
		if bold {
			fallback = boldFont
		}
		face, err := newFallbackFace(preferred, fallback, size)
		if err == nil {
			faces.all = append(faces.all, face)
		}
		return face, err
	}
	if faces.title, err = makeFace(30, true); err != nil {
		faces.Close()
		return nil, err
	}
	if faces.subtitle, err = makeFace(17, false); err != nil {
		faces.Close()
		return nil, err
	}
	if faces.header, err = makeFace(17, true); err != nil {
		faces.Close()
		return nil, err
	}
	if faces.body, err = makeFace(16, false); err != nil {
		faces.Close()
		return nil, err
	}
	if faces.small, err = makeFace(15, false); err != nil {
		faces.Close()
		return nil, err
	}
	if fallback, ok := faces.body.(*fallbackFace); ok {
		faces.cjk = fallback.primarySupports('测')
	} else {
		_, faces.cjk = faces.body.GlyphAdvance('测')
	}
	return faces, nil
}

// Close 释放本次渲染创建的所有字体 Face。
func (f *reportFaces) Close() {
	for _, face := range f.all {
		_ = face.Close()
	}
}

// newFallbackFace 以 preferred 为首选字体、embedded 为兜底字体创建同字号字体链。
func newFallbackFace(preferred, embedded *opentype.Font, size float64) (font.Face, error) {
	options := &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull}
	primary, err := opentype.NewFace(preferred, options)
	if err != nil {
		return nil, err
	}
	if preferred == embedded {
		return &fallbackFace{faces: []font.Face{primary}}, nil
	}
	fallback, err := opentype.NewFace(embedded, options)
	if err != nil {
		_ = primary.Close()
		return nil, err
	}
	return &fallbackFace{faces: []font.Face{primary, fallback}}, nil
}

// fallbackFace 为 x/image/font.Drawer 提供逐字形字体回退。所有字体使用相同字号，
// 因此切换字体时不会破坏行高；全部字体都缺少字形时使用可移植的 ASCII 问号。
type fallbackFace struct {
	faces []font.Face
}

// primarySupports 直接探测首选原字体，不经过 fallbackFace.GlyphAdvance 的 '?' 替代，
// 因此不会把缺失的中文字形误判为可用。
func (f *fallbackFace) primarySupports(r rune) bool {
	_, ok := f.faces[0].GlyphAdvance(r)
	return ok
}

func (f *fallbackFace) Close() error {
	var first error
	for _, face := range f.faces {
		if err := face.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (f *fallbackFace) Glyph(dot fixed.Point26_6, r rune) (image.Rectangle, image.Image, image.Point, fixed.Int26_6, bool) {
	for _, face := range f.faces {
		if rectangle, mask, point, advance, ok := face.Glyph(dot, r); ok {
			return rectangle, mask, point, advance, true
		}
	}
	return f.faces[len(f.faces)-1].Glyph(dot, '?')
}

func (f *fallbackFace) GlyphBounds(r rune) (fixed.Rectangle26_6, fixed.Int26_6, bool) {
	for _, face := range f.faces {
		if bounds, advance, ok := face.GlyphBounds(r); ok {
			return bounds, advance, true
		}
	}
	return f.faces[len(f.faces)-1].GlyphBounds('?')
}

func (f *fallbackFace) GlyphAdvance(r rune) (fixed.Int26_6, bool) {
	for _, face := range f.faces {
		if advance, ok := face.GlyphAdvance(r); ok {
			return advance, true
		}
	}
	return f.faces[len(f.faces)-1].GlyphAdvance('?')
}

func (f *fallbackFace) Kern(r0, r1 rune) fixed.Int26_6 {
	for _, face := range f.faces {
		if _, ok := face.GlyphAdvance(r0); !ok {
			continue
		}
		if _, ok := face.GlyphAdvance(r1); ok {
			return face.Kern(r0, r1)
		}
	}
	return 0
}

func (f *fallbackFace) Metrics() font.Metrics {
	return f.faces[0].Metrics()
}

var _ font.Face = (*fallbackFace)(nil)
var _ io.Closer = (*fallbackFace)(nil)
