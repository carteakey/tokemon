package main

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
)

func main() {
	if len(os.Args) == 3 && os.Args[1] == "--check" {
		checkAlpha(os.Args[2])
		return
	}
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: chromakey input.png output.png | chromakey --check image.png")
		os.Exit(2)
	}
	input, err := os.Open(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer input.Close()
	source, _, err := image.Decode(input)
	if err != nil {
		panic(err)
	}
	result := image.NewNRGBA(source.Bounds())
	for y := source.Bounds().Min.Y; y < source.Bounds().Max.Y; y++ {
		for x := source.Bounds().Min.X; x < source.Bounds().Max.X; x++ {
			r, g, b, a := source.At(x, y).RGBA()
			red, green, blue := uint8(r>>8), uint8(g>>8), uint8(b>>8)
			alpha := uint8(a >> 8)
			if green > 100 && int(green) > int(red)+25 && int(green) > int(blue)+25 {
				red, green, blue = 0, 0, 0
				alpha = 0
			}
			result.SetNRGBA(x, y, color.NRGBA{R: red, G: green, B: blue, A: alpha})
		}
	}
	output, err := os.Create(os.Args[2])
	if err != nil {
		panic(err)
	}
	defer output.Close()
	if err := png.Encode(output, result); err != nil {
		panic(err)
	}
}

func checkAlpha(path string) {
	input, err := os.Open(path)
	if err != nil {
		panic(err)
	}
	defer input.Close()
	imageData, _, err := image.Decode(input)
	if err != nil {
		panic(err)
	}
	transparent := 0
	for y := imageData.Bounds().Min.Y; y < imageData.Bounds().Max.Y; y++ {
		for x := imageData.Bounds().Min.X; x < imageData.Bounds().Max.X; x++ {
			_, _, _, alpha := imageData.At(x, y).RGBA()
			if alpha == 0 {
				transparent++
			}
		}
	}
	total := imageData.Bounds().Dx() * imageData.Bounds().Dy()
	fmt.Printf("%s: %d/%d pixels transparent (%.1f%%)\n", path, transparent, total, float64(transparent)*100/float64(total))
}
