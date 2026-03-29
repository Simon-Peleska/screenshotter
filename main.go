package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/png"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/otiai10/gosseract/v2"
)

func main() {
	regionFlag := flag.Bool("region", false, "select a region interactively")
	ocrFlag := flag.Bool("ocr", false, "run OCR on the screenshot and copy text to clipboard (requires libtesseract)")
	outDir := flag.String("dir", screenshotDir(), "directory to save screenshots")
	flag.Parse()

	if err := os.MkdirAll(*outDir, 0755); err != nil {
		log.Fatalf("create output directory %q: %v", *outDir, err)
	}

	// Connect to Wayland once for the screenshot.
	wl, err := connect()
	if err != nil {
		log.Fatalf("wayland connect: %v", err)
	}
	reg, err := getRegistry(wl)
	if err != nil {
		wl.close()
		log.Fatalf("get registry: %v", err)
	}

	// Gather global positions of all outputs so we can convert the selection's
	// global coordinates to output-local coordinates for cropping.
	outputs, err := gatherOutputGeoms(wl, reg)
	if err != nil {
		wl.close()
		log.Fatalf("gather outputs: %v", err)
	}

	// Region selection runs before we capture, because we need the geometry first.
	var globalRegion *image.Rectangle
	if *regionFlag {
		r, err := selectRegionWayland(outputs)
		if err != nil {
			wl.close()
			if errors.Is(err, errCancelled) {
				os.Exit(0)
			}
			log.Fatalf("region select: %v", err)
		}
		globalRegion = &r
	}

	// Pick the output that best contains the selected region, and convert
	// the region from global compositor coords to output-local coords.
	outputName, localRegion := pickOutput(outputs, globalRegion)

	img, err := screenshot(wl, reg, outputName, localRegion)
	wl.close()
	if err != nil {
		log.Fatalf("screenshot: %v", err)
	}

	if *ocrFlag {
		text, err := runOCR(img)
		if err != nil {
			log.Fatalf("ocr: %v", err)
		}
		if err := setClipboard(text); err != nil {
			log.Fatalf("clipboard: %v", err)
		}
		fmt.Printf("OCR text copied to clipboard (%d chars)\n", len(text))
		return
	}

	outPath := filepath.Join(*outDir, filename())
	if err := savePNG(img, outPath); err != nil {
		log.Fatalf("save: %v", err)
	}
	fmt.Println(outPath)
}

// screenshotDir returns the default screenshot save directory.
func screenshotDir() string {
	if d := os.Getenv("XDG_PICTURES_DIR"); d != "" {
		return filepath.Join(d, "Screenshots")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, "Pictures", "Screenshots")
}

// filename generates a timestamped PNG filename.
func filename() string {
	return time.Now().Format("2006-01-02_15-04-05") + ".png"
}

// savePNG encodes img as PNG and writes it to path.
func savePNG(img image.Image, path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

// pickOutput returns the output that best overlaps globalRegion (most overlap area),
// and the region converted to output-local coordinates. If globalRegion is nil,
// the first output is returned with a nil local region.
func pickOutput(outputs []*outputGeom, globalRegion *image.Rectangle) (uint32, *image.Rectangle) {
	best := outputs[0]
	if globalRegion != nil && len(outputs) > 1 {
		bestArea := 0
		for _, o := range outputs {
			overlap := image.Rect(o.x, o.y, o.x+o.w, o.y+o.h).Intersect(*globalRegion)
			if a := overlap.Dx() * overlap.Dy(); a > bestArea {
				bestArea = a
				best = o
			}
		}
	}
	if globalRegion == nil {
		return best.regName, nil
	}
	local := globalRegion.Sub(image.Pt(best.x, best.y))
	return best.regName, &local
}

// minConfidence is the minimum per-word confidence (0–100) required to include
// a word in the OCR output. Words below this threshold are dropped to avoid
// garbage characters from images with no text or cut-off text.
const minConfidence = 70.0

// runOCR encodes the image to PNG bytes and runs OCR via gosseract (CGO/libtesseract).
// Only words with confidence >= minConfidence are included in the result.
func runOCR(img image.Image) (string, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return "", fmt.Errorf("encode for OCR: %w", err)
	}

	client := gosseract.NewClient()
	defer client.Close()

	if err := client.SetImageFromBytes(buf.Bytes()); err != nil {
		return "", fmt.Errorf("gosseract set image: %w", err)
	}

	boxes, err := client.GetBoundingBoxesVerbose()
	if err != nil {
		return "", fmt.Errorf("gosseract bounding boxes: %w", err)
	}

	// Reconstruct text line by line, skipping low-confidence words.
	// GetBoundingBoxesVerbose returns words in reading order grouped by
	// block/par/line numbers.
	var lines []string
	var currentLine []string
	currentLineNum := -1
	currentParNum := -1
	currentBlockNum := -1

	for _, box := range boxes {
		if box.Word == "" {
			continue
		}
		if box.Confidence < minConfidence {
			continue
		}
		newLine := box.BlockNum != currentBlockNum || box.ParNum != currentParNum || box.LineNum != currentLineNum
		if newLine {
			if len(currentLine) > 0 {
				lines = append(lines, strings.Join(currentLine, " "))
			}
			currentLine = nil
			currentBlockNum = box.BlockNum
			currentParNum = box.ParNum
			currentLineNum = box.LineNum
		}
		currentLine = append(currentLine, box.Word)
	}
	if len(currentLine) > 0 {
		lines = append(lines, strings.Join(currentLine, " "))
	}

	return strings.Join(lines, "\n"), nil
}
