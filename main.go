package main

import (
	"bytes"
	"flag"
	"fmt"
	"image"
	"image/png"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/otiai10/gosseract/v2"
)

func main() {
	regionFlag := flag.Bool("region", false, "select a region interactively using slurp (requires slurp in PATH)")
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

	// Gather global positions of all outputs so we can convert slurp's global
	// coordinates to output-local coordinates for cropping.
	outputs, err := gatherOutputGeoms(wl, reg)
	if err != nil {
		wl.close()
		log.Fatalf("gather outputs: %v", err)
	}

	// Region selection runs before we capture, because we need the geometry first.
	var globalRegion *image.Rectangle
	if *regionFlag {
		r, err := selectRegion()
		if err != nil {
			wl.close()
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

// selectRegion runs slurp and parses the selected geometry.
// slurp outputs a string like "100,200 800x600" or "100,200 800x600\n".
func selectRegion() (image.Rectangle, error) {
	out, err := exec.Command("slurp").Output()
	if err != nil {
		return image.Rectangle{}, fmt.Errorf("slurp: %w", err)
	}
	return parseGeometry(strings.TrimSpace(string(out)))
}

// parseGeometry parses a geometry string in the format "x,y WxH".
// Coordinates may be floats (slurp outputs floats on HiDPI displays).
func parseGeometry(s string) (image.Rectangle, error) {
	// Expected format: "X,Y WxH"
	parts := strings.Fields(s)
	if len(parts) != 2 {
		return image.Rectangle{}, fmt.Errorf("unexpected geometry format %q", s)
	}
	xy := strings.SplitN(parts[0], ",", 2)
	wh := strings.SplitN(parts[1], "x", 2)
	if len(xy) != 2 || len(wh) != 2 {
		return image.Rectangle{}, fmt.Errorf("unexpected geometry format %q", s)
	}
	x, err1 := parseCoord(xy[0])
	y, err2 := parseCoord(xy[1])
	w, err3 := parseCoord(wh[0])
	h, err4 := parseCoord(wh[1])
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		return image.Rectangle{}, fmt.Errorf("non-numeric values in geometry %q", s)
	}
	return image.Rect(x, y, x+w, y+h), nil
}

func parseCoord(s string) (int, error) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	return int(math.Round(f)), nil
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

// runOCR encodes the image to PNG bytes and runs OCR via gosseract (CGO/libtesseract).
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

	text, err := client.Text()
	if err != nil {
		return "", fmt.Errorf("gosseract text: %w", err)
	}
	return strings.TrimSpace(text), nil
}
