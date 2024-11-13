package main

import (
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	"github.com/mjl-/mox/dns"
	"github.com/mjl-/mox/dnsbl"
	"context"
	"net"
	"net/http"
	"math/rand"
	"path/filepath"
	"fmt"
	"time"
	"log"
	"log/slog"
	"os"
	"bufio"
	"strings"
	"slices"
	"sync"
)

const VERSION = "1.0.0"

var bannedIPs []string
var bannedMimes []string

type UserInfo struct {
	UploadsToday	int
	LastUpload	int64
}
var users = make(map[string]*UserInfo)
var mu sync.Mutex

func getFullHostURL(c *gin.Context) string {
	scheme := "http"
	if c.Request.TLS != nil {
		scheme = "https"
	}

	host := c.Request.Host 

	return scheme + "://" + host
}

func generateRandomID() string {
	rand.Seed(time.Now().UnixNano())
	chars := "abcdefghijklmnopqrstuvwxyz0123456789"
	result := make([]byte, 5)
	for i := range result {
		result[i] = chars[rand.Intn(len(chars))]
	}
	return string(result)
}

func ensureDirExists(dir string) error {
	_, err := os.Stat(dir)
	if os.IsNotExist(err) {
		err = os.MkdirAll(dir, os.ModePerm)
	}
	return err
}

func ensureFileExists(filename string) error {
	if _, err := os.Stat(filename); os.IsNotExist(err) {
		file, err := os.Create(filename)
		if err != nil {
			return err
		}
		defer file.Close()
	}
	return nil
}

func reloadBanLists() error {
	if err := ensureFileExists("banned.txt"); err != nil {
		return err
	}

	if err := ensureFileExists("banned-mimes.txt"); err != nil {
		return err 
	}

	bannedIPs = nil
	bannedMimes = nil

	if err := readBannedIPs("banned.txt"); err != nil {
		return err
	}

	if err := readBannedMimes("banned-mimes.txt"); err != nil {
		return err
	}

	return nil
}

func readBannedIPs(filename string) error {
	file, err := os.Open(filename)
	if err != nil {
		return fmt.Errorf("failed to open %s: %v", filename, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		bannedIPs = append(bannedIPs, line)
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error: %v", err)
	}

	return nil
}

func readBannedMimes(filename string) error {
	file, err := os.Open(filename)
	if err != nil {
		return fmt.Errorf("failed to open %s: %v", filename, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		bannedMimes = append(bannedMimes, line)
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error: %v", err)
	}

	return nil
}

func detectMimeType(filePath string) string {
	file, err := os.Open(filePath)
	if err != nil {
		return "application/octet-stream"
	}
	defer file.Close()

	buf := make([]byte, 512)
	_, err = file.Read(buf)
	if err != nil {
		return "application/octet-stream"
	}

	return http.DetectContentType(buf)
}

func handleFile(c *gin.Context) {
	requestStart := time.Now().UnixNano()
	userAgent := c.GetHeader("User-Agent")
	requestedWith := c.GetHeader("X-Requested-With")
	forwardedFor := c.GetHeader("X-Forwarded-For")
	ip := ""
	if forwardedFor != "" {
		ip = strings.Split(forwardedFor, ",")[0]
	} else {
		ip = c.ClientIP()
	}

	err500 := gin.H{
		"error": "Error interno (500)",
	}

	if err := reloadBanLists(); err != nil {
		log.Fatal("FATAL> unable to load ban lists")
		c.JSON(500, err500)
		return
	}
	if slices.Contains(bannedIPs, ip) {
		log.Printf("INFO> %s is banned; request discarded")
		c.JSON(403, gin.H{
		"error": "Tu dirección IP fue baneada",
		"ip": ip,
		"info": "Contacta a admin@ichoria.org para más info.",
		})
		return
	}

	ctx := context.Background()
	resolver := dns.StrictResolver{}
	status, reason, _ := dnsbl.Lookup(
		ctx,
		slog.Default(),
		resolver,
		dns.Domain{ ASCII: "all.s5h.net" },
		net.ParseIP(ip),
	)
	switch status {
	case dnsbl.StatusFail:
		log.Printf("WARN> %s's request rejected by dnsbl", ip)
		c.JSON(403, gin.H{
			"error": "Dirección IP listada como spam",
			"reason": reason,
		})
		return
	default:
		break
	}

	mu.Lock()
	defer mu.Unlock()

	if _, ok := users[ip]; ok {
		user := users[ip];
		lastUploadDay := time.Unix(0, user.LastUpload).
			Format("2006-01-02")
		currentDay := time.Unix(0, requestStart).
			Format("2006-01-02")

		if lastUploadDay == currentDay && user.UploadsToday >= 100 {
			log.Printf("INFO> Applied rate limiting to %s", ip)
			c.JSON(429, gin.H{
			"error": "Has alcanzado el límite de subidas por hoy",
			})
			return
		}

		timeSinceLastUpload := time.Now().Unix() - user.LastUpload/1e9
		if timeSinceLastUpload < 10 {
			log.Printf("INFO> %s tried uploading too quickly", ip)
			c.JSON(429, gin.H{
			"error": "Debes esperar 10 segundos entre subidas",
			})
			return
		}

		if lastUploadDay == currentDay {
			user.UploadsToday++
		} else {
			user.UploadsToday = 1
		}
		user.LastUpload = requestStart
	} else {
		users[ip] = &UserInfo{
			UploadsToday:	1,
			LastUpload:	requestStart,
		}
	}


	err := c.Request.ParseMultipartForm(10 << 20)
	if err != nil {
		c.JSON(400, gin.H{
			"error": "Datos multiparte malformados",
		})
		return
	}

	files := c.Request.MultipartForm.File["files"]
	if len(files) == 0 {
		c.JSON(400, gin.H{
			"error": "No se subieron archivos",
		})
		return
	}
	if len(files) > 3 {
		c.JSON(400, gin.H{
		"error": "Solo se permite subir un máximo de 3 archivos",
		})
		return
	}

	var totalSize int64
	var fileDetails []gin.H

	for _, file := range files {
		fileID := generateRandomID()
		filename := strings.ReplaceAll(file.Filename, " ", "_")
		mime := file.Header.Get("Content-Type")

		if slices.Contains(bannedMimes, mime) {
			log.Printf(
				"WARN> %s tried to upload forbidden " +
				"MIME type %s",
				ip,
				mime,
			)
			c.JSON(400, gin.H{
				"error": "Tipo de archivo no permitido",
				"mime": mime,
			})
			return
		}

		log.Printf(
			"INFO> %s is uploading f/%s/%s",
			ip, fileID, filename,
		)

		totalSize += file.Size
		fileDetails = append(fileDetails, gin.H{
			"fileID":   fileID,
			"filename": filename,
			"mime":     mime,
			"size":     file.Size,
		})
	}

	const maxTotalSize = 100 << 20
	if totalSize > maxTotalSize {
		log.Printf("INFO> %s's request was denied (file size)", ip)
		c.JSON(400, gin.H{
			"error": "Solo se permiten max. 100MB por subida",
		})
		return
	}

	baseDir := "f/"
	err = ensureDirExists(baseDir)
	if err != nil {
		log.Fatal("FATAL> unable to write to directory f/")
		c.JSON(500, err500)
		return
	}

	for i, file := range files {
		fileID := fileDetails[i]["fileID"].(string)
		uploadDir := filepath.Join(baseDir, fileID)

		err = ensureDirExists(uploadDir)
		if err != nil {
			c.JSON(500, err500)
			return
		}

		filename := fileDetails[i]["filename"].(string)
		dstPath := filepath.Join(uploadDir, filename)

		dstFile, err := os.Create(dstPath)
		if err != nil {
			log.Fatal("FATAL> unable to create uploaded file")
			c.JSON(500, err500)
			return
		}
		defer dstFile.Close()

		srcFile, err := file.Open()
		if err != nil {
			log.Fatal("FATAL> unable to read uploaded file")
			c.JSON(500, err500)
			return
		}
		defer srcFile.Close()

		_, err = dstFile.ReadFrom(srcFile)
		if err != nil {
			log.Fatal("FATAL> unable to copy uploaded file")
			c.JSON(500, err500)
			return
		}
	}

	host := getFullHostURL(c)
	requestEnd := time.Now().UnixNano()
	diffSeconds := float64(requestEnd - requestStart) / 1_000_000_000.0
	log.Printf(
		"INFO> %s's request was accepted (%d file/s)",
		ip, len(files),
	)

	if strings.Contains(userAgent, "curl") ||
	userAgent == "" ||
	requestedWith == "NEO" ||
	requestedWith == "Bear" {
		var links []string
		for i, _ := range files {
			link := fmt.Sprintf(
				"%s/f/%s/%s",
				host,
				fileDetails[i]["fileID"],
				fileDetails[i]["filename"],
			)
			links = append(links, link)
		}
		responseString := strings.Join(links, "\n") + "\n"
		c.Header("Content-Type", "text/plain")
		c.String(200, responseString)
		return
	}

	c.HTML(200, "upload.html", gin.H{
		"Host":		host,
		"Version":	VERSION,
		"IP":		ip,
		"Files":	fileDetails,
		"RequestTime":	fmt.Sprintf("%.6f", diffSeconds),
	})
}

func renderFile(c *gin.Context) {
	id := c.Param("id")
	filename := c.Param("filename")
	
	filePath := fmt.Sprintf("./f/%s/%s", id, filename)

	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		c.HTML(404, "not_found.html", gin.H{
			"Version": VERSION,
		})
		return
	}

	contentType := detectMimeType(filePath)
	c.Header("Content-Type", contentType)
	c.File(filePath)
}

func main() {
	if err := godotenv.Load(); err != nil {
		log.Fatal("Unable to load .env file: ", err)
		return
	}
	port := os.Getenv("PORT")
	csp := os.Getenv("CSP_HEADER")
	
	log.Printf("Starting neosaki %s on port %s", VERSION, port)
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Static("/static", "static/")
	r.StaticFile("/favicon.ico", "static/favicon.ico")
	r.LoadHTMLGlob("views/*")

	ensureFileExists("neosaki.log")
	if err := reloadBanLists(); err != nil {
		log.Fatal("FATAL Unable to load ban lists:", err)
		return
	}


	r.Use(func(c *gin.Context) {
		c.Header("Content-Security-Policy", csp)
		c.Next()
	})	

	r.GET("/", func(c *gin.Context) {
		host := getFullHostURL(c)

		c.HTML(200, "index.html", gin.H{
			"Host":		host,
			"Version":	VERSION,
		})
	})

	r.POST("/", handleFile)
	r.POST("/upload", handleFile)

	r.GET("/f/:id/:filename", renderFile)
	
	r.NoRoute(func(c *gin.Context) {
		c.HTML(404, "not_found.html", gin.H{
			"Version": VERSION,
		})
	})

	file, err := os.OpenFile(
		"neosaki.log",
		os.O_APPEND|os.O_CREATE|os.O_WRONLY,
		0666,
	)
	if err != nil {
		log.Fatal(err)
		return
	}
	log.SetOutput(file)

	if err := r.Run(":" + port); err != nil {
		log.Fatal("FATAL Unable to listen on the port: ", err)
		return
	}

}
