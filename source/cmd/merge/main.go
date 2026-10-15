package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type Part struct {
	File  string `json:"file"`
	Entry string `json:"entry"`
	SHA   string `json:"sha256"`
	Size  int64  `json:"size"`
}
type Manifest struct {
	Output string `json:"output"`
	SHA    string `json:"sha256"`
	Size   int64  `json:"size"`
	Parts  []Part `json:"parts"`
}

func safeName(n string) bool {
	return n != "" && n == filepath.Base(n) && !strings.ContainsAny(n, "/\\:") && n != "." && n != ".."
}
func fileSHA(p string) (string, error) {
	f, e := os.Open(p)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha256.New()
	_, e = io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), e
}
func merge(dir string) (string, error) {
	raw, e := os.ReadFile(filepath.Join(dir, "Healthtara_Parts_Manifest.json"))
	if e != nil {
		return "", errors.New("Healthtara_Parts_Manifest.json belum ditemukan. Ekstrak paket Penggabung terlebih dahulu.")
	}
	if len(raw) > 65536 {
		return "", errors.New("Manifest terlalu besar")
	}
	var m Manifest
	if e = json.Unmarshal(raw, &m); e != nil {
		return "", e
	}
	if !safeName(m.Output) || !strings.HasSuffix(m.Output, ".zip") || len(m.Parts) < 1 || len(m.Parts) > 100 || m.Size <= 0 || m.Size > 5<<30 {
		return "", errors.New("Manifest tidak valid")
	}
	if _, e = hex.DecodeString(m.SHA); e != nil || len(m.SHA) != 64 {
		return "", errors.New("Hash manifest tidak valid")
	}
	output := filepath.Join(dir, m.Output)
	if _, e = os.Stat(output); e == nil {
		sha, e := fileSHA(output)
		if e == nil && sha == m.SHA {
			return output, nil
		}
		return "", fmt.Errorf("%s sudah ada dengan isi berbeda. Pindahkan file lama dahulu; penggabung tidak menimpanya.", m.Output)
	}
	temp, e := os.CreateTemp(dir, "Healthtara_gabungan_*.tmp")
	if e != nil {
		return "", e
	}
	defer os.Remove(temp.Name())
	hash := sha256.New()
	writer := io.MultiWriter(temp, hash)
	var total int64
	seen := map[string]bool{}
	for i, p := range m.Parts {
		if !safeName(p.File) || !safeName(p.Entry) || seen[p.File] || p.Size <= 0 || p.Size > 49_000_000 || len(p.SHA) != 64 {
			temp.Close()
			return "", errors.New("Bagian manifest tidak valid")
		}
		seen[p.File] = true
		zipPath := filepath.Join(dir, p.File)
		if info, e := os.Stat(zipPath); e != nil || info.Size() > 50_000_000 {
			temp.Close()
			return "", fmt.Errorf("Bagian %d belum ditemukan atau melebihi 50 MB: %s", i+1, p.File)
		}
		z, e := zip.OpenReader(zipPath)
		if e != nil {
			temp.Close()
			return "", fmt.Errorf("ZIP %s rusak: %w", p.File, e)
		}
		var target *zip.File
		for _, f := range z.File {
			if f.Name == p.Entry {
				if target != nil {
					z.Close()
					temp.Close()
					return "", errors.New("Entri bagian ganda")
				}
				target = f
			}
		}
		if target == nil || target.UncompressedSize64 != uint64(p.Size) {
			z.Close()
			temp.Close()
			return "", fmt.Errorf("Isi ZIP %s tidak sesuai manifest", p.File)
		}
		reader, e := target.Open()
		if e != nil {
			z.Close()
			temp.Close()
			return "", e
		}
		partHash := sha256.New()
		n, e := io.Copy(io.MultiWriter(writer, partHash), io.LimitReader(reader, p.Size+1))
		reader.Close()
		z.Close()
		if e != nil || n != p.Size || hex.EncodeToString(partHash.Sum(nil)) != p.SHA {
			temp.Close()
			return "", fmt.Errorf("Bagian %d gagal pemeriksaan SHA256. Unduh ulang %s.", i+1, p.File)
		}
		total += n
		fmt.Printf("Bagian %d dari %d terverifikasi\n", i+1, len(m.Parts))
	}
	if total != m.Size || hex.EncodeToString(hash.Sum(nil)) != m.SHA {
		temp.Close()
		return "", errors.New("Hash atau ukuran ZIP gabungan tidak cocok")
	}
	if e = temp.Sync(); e != nil {
		temp.Close()
		return "", e
	}
	if e = temp.Close(); e != nil {
		return "", e
	}
	z, e := zip.OpenReader(temp.Name())
	if e != nil {
		return "", errors.New("Hasil gabungan bukan ZIP lengkap yang valid")
	}
	z.Close()
	if e = os.Rename(temp.Name(), output); e != nil {
		return "", e
	}
	return output, nil
}
func main() {
	exe, _ := os.Executable()
	dir := flag.String("dir", filepath.Dir(exe), "Folder containing part ZIP files and manifest")
	flag.Parse()
	p, e := merge(*dir)
	if e != nil {
		notify("Penggabungan belum berhasil", e.Error())
		os.Exit(1)
	}
	notify("ZIP Healthtara siap", "Semua bagian dan ZIP lengkap lolos SHA256.\n\n"+p+"\n\nEkstrak ZIP ini, lalu buka Healthtara.exe.")
}
