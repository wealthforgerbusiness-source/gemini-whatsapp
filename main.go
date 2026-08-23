package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/glebarez/go-sqlite"
	"github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/container"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

type Config struct {
	AdminGlobal     bool
	BotActif        bool
	LimiteTokens    int64
	TokensConsommes int64
	PromptBusiness  string
}

var (
	waClient   *whatsmeow.Client
	sheetsSvc  *sheets.Service
	sheetID    string
	geminiKey  string
	currentQR  string
	qrMutex    sync.Mutex
	isLoggedIn bool
)

func main() {
	sheetID = os.Getenv("GOOGLE_SHEET_ID")
	geminiKey = os.Getenv("GEMINI_API_KEY")
	credJSON := os.Getenv("GOOGLE_CREDENTIALS_JSON")

	if sheetID == "" || geminiKey == "" || credJSON == "" {
		log.Fatal("Erreur : Variables d'environnement GOOGLE_SHEET_ID, GEMINI_API_KEY et GOOGLE_CREDENTIALS_JSON obligatoires.")
	}

	ctx := context.Background()
	var err error
	sheetsSvc, err = sheets.NewService(ctx, option.WithCredentialsJSON([]byte(credJSON)))
	if err != nil {
		log.Fatalf("Impossible d'initialiser Google Sheets: %v", err)
	}

	// Initialisation de WhatsApp Whatsmeow avec base SQLite
	dbContainer, err := container.New("sqlite", "file:whatsapp.db?_pragma=foreign_keys(1)", waLog.Stdout("Database", "WARN", true))
	if err != nil {
		log.Fatalf("Erreur base de données WhatsApp: %v", err)
	}

	deviceStore, err := dbContainer.GetFirstDevice()
	if err != nil {
		log.Fatalf("Erreur device store: %v", err)
	}

	clientLog := waLog.Stdout("Client", "WARN", true)
	waClient = whatsmeow.NewClient(deviceStore, clientLog)
	waClient.AddEventHandler(eventHandler)

	if waClient.Store.ID == nil {
		qrChan, _ := waClient.GetQRChannel(context.Background())
		err = waClient.Connect()
		if err != nil {
			log.Fatalf("Erreur connexion WhatsApp: %v", err)
		}
		go handleQR(qrChan)
	} else {
		isLoggedIn = true
		err = waClient.Connect()
		if err != nil {
			log.Fatalf("Erreur reconnexion WhatsApp: %v", err)
		}
	}

	// Serveur Web pour Tableau de bord
	http.HandleFunc("/", serveDashboard)
	http.HandleFunc("/api/status", handleAPIStatus)
	http.HandleFunc("/api/toggle", handleAPIToggle)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("Serveur démarré sur le port %s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

func handleQR(qrChan <-chan whatsmeow.QRChannelItem) {
	for evt := range qrChan {
		if evt.Event == "code" {
			png, err := qrcode.Encode(evt.Code, qrcode.Medium, 256)
			if err == nil {
				qrMutex.Lock()
				currentQR = "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
				isLoggedIn = false
				qrMutex.Unlock()
			}
		} else if evt.Event == "success" {
			qrMutex.Lock()
			currentQR = ""
			isLoggedIn = true
			qrMutex.Unlock()
		}
	}
}

func getConfig() (Config, error) {
	resp, err := sheetsSvc.Spreadsheets.Values.Get(sheetID, "Config!B1:B7").Do()
	if err != nil || len(resp.Values) < 7 {
		return Config{}, fmt.Errorf("erreur lecture config: %v", err)
	}

	admin := strings.ToUpper(fmt.Sprintf("%v", resp.Values[0][0])) == "TRUE"
	actif := strings.ToUpper(fmt.Sprintf("%v", resp.Values[1][0])) == "TRUE"
	limite, _ := strconv.ParseInt(fmt.Sprintf("%v", resp.Values[2][0]), 10, 64)
	consommes, _ := strconv.ParseInt(fmt.Sprintf("%v", resp.Values[3][0]), 10, 64)
	prompt := fmt.Sprintf("%v", resp.Values[6][0])

	return Config{
		AdminGlobal:     admin,
		BotActif:        actif,
		LimiteTokens:    limite,
		TokensConsommes: consommes,
		PromptBusiness:  prompt,
	}, nil
}

func eventHandler(evt interface{}) {
	switch v := evt.(type) {
	case *events.Message:
		if v.Info.IsFromMe || v.Message.GetConversation() == "" {
			return
		}
		sender := v.Info.Sender.User
		msgText := v.Message.GetConversation()

		cfg, err := getConfig()
		if err != nil || !cfg.AdminGlobal || !cfg.BotActif {
			return
		}

		if cfg.TokensConsommes >= cfg.LimiteTokens {
			log.Println("Limite de tokens atteinte, message ignoré.")
			return
		}

		go processAIResponse(sender, msgText, cfg)
	}
}

func processAIResponse(sender, userMessage string, cfg Config) {
	history := getHistory(sender)
	
	// Préparation de la requête Gemini API
	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/gemini-1.5-flash:generateContent?key=%s", geminiKey)

	type Content struct {
		Role  string `json:"role"`
		Parts []map[string]string `json:"parts"`
	}

	var contents []Content
	contents = append(contents, Content{Role: "user", Parts: []map[string]string{{"text": "INSTRUCTIONS DU BUSINESS: " + cfg.PromptBusiness}}})
	contents = append(contents, Content{Role: "model", Parts: []map[string]string{{"text": "Instructions bien reçues."}}})

	for _, h := range history {
		contents = append(contents, Content{Role: h["role"], Parts: []map[string]string{{"text": h["text"]}}})
	}
	contents = append(contents, Content{Role: "user", Parts: []map[string]string{{"text": userMessage}}})

	reqBody, _ := json.Marshal(map[string]interface{}{"contents": contents})
	resp, err := http.Post(url, "application/json", bytes.NewBuffer(reqBody))
	if err != nil {
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	var geminiResp struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		UsageMetadata struct {
			TotalTokenCount int64 `json:"totalTokenCount"`
		} `json:"usageMetadata"`
	}

	json.Unmarshal(body, &geminiResp)

	if len(geminiResp.Candidates) > 0 {
		aiReply := geminiResp.Candidates[0].Content.Parts[0].Text
		tokensUsed := geminiResp.UsageMetadata.TotalTokenCount

		// Envoyer sur WhatsApp
		waClient.SendMessage(context.Background(), waClient.Store.ID, &whatsmeow.Message{
			Conversation: &aiReply,
		})

		// Sauvegarder dans Google Sheet
		saveHistory(sender, userMessage, aiReply)
		updateTokens(cfg.TokensConsommes + tokensUsed)
	}
}

func getHistory(sender string) []map[string]string {
	resp, err := sheetsSvc.Spreadsheets.Values.Get(sheetID, "History!A2:D").Do()
	var history []map[string]string
	if err != nil {
		return history
	}

	for _, row := range resp.Values {
		if len(row) >= 4 && fmt.Sprintf("%v", row[1]) == sender {
			history = append(history, map[string]string{
				"role": fmt.Sprintf("%v", row[2]),
				"text": fmt.Sprintf("%v", row[3]),
			})
		}
	}
	return history
}

func saveHistory(sender, userMsg, aiMsg string) {
	now := time.Now().Format("2006-01-02 15:04:05")
	vr := &sheets.ValueRange{
		Values: [][]interface{}{
			{now, sender, "user", userMsg},
			{now, sender, "model", aiMsg},
		},
	}
	sheetsSvc.Spreadsheets.Values.Append(sheetID, "History!A:D", vr).ValueInputOption("USER_ENTERED").Do()
}

func updateTokens(nouveauTotal int64) {
	vr := &sheets.ValueRange{Values: [][]interface{}{{nouveauTotal}}}
	sheetsSvc.Spreadsheets.Values.Update(sheetID, "Config!B4", vr).ValueInputOption("USER_ENTERED").Do()
}

func serveDashboard(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "index.html")
}

func handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	cfg, _ := getConfig()
	qrMutex.Lock()
	qr := currentQR
	qrMutex.Unlock()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"admin":     cfg.AdminGlobal,
		"actif":     cfg.BotActif,
		"limite":    cfg.LimiteTokens,
		"consommes": cfg.TokensConsommes,
		"connected": isLoggedIn,
		"qr":        qr,
	})
}

func handleAPIToggle(w http.ResponseWriter, r *http.Request) {
	cfg, err := getConfig()
	if err != nil || !cfg.AdminGlobal {
		http.Error(w, "Opération non autorisée par l'Admin", http.StatusForbidden)
		return
	}

	nouvelEtat := !cfg.BotActif
	vr := &sheets.ValueRange{Values: [][]interface{}{{nouvelEtat}}}
	sheetsSvc.Spreadsheets.Values.Update(sheetID, "Config!B2", vr).ValueInputOption("USER_ENTERED").Do()

	json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "actif": nouvelEtat})
}
