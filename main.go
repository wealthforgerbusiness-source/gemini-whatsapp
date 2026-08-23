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

	_ "github.com/mattn/go-sqlite3"
	"github.com/skip2/go-qrcode"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
	"google.golang.org/protobuf/proto"
)

type Config struct {
	AdminGlobal     bool
	BotActif        bool
	LimiteTokens    int64
	TokensConsommes int64
	PromptBusiness  string
}

var (
	waClient  *whatsmeow.Client
	sheetsSvc *sheets.Service

	sheetID   string
	geminiKey string

	currentQR string

	qrMutex sync.RWMutex

	statusMutex sync.RWMutex
	isLoggedIn  bool

	tokenMutex sync.Mutex

	httpClient = &http.Client{
		Timeout: 60 * time.Second,
	}
)

func main() {
	log.Println("======================================")
	log.Println("       WHATSAPP AI BOT - START")
	log.Println("======================================")

	// ==================================================
	// VARIABLES D'ENVIRONNEMENT
	// ==================================================

	sheetID = strings.TrimSpace(
		os.Getenv("GOOGLE_SHEET_ID"),
	)

	geminiKey = strings.TrimSpace(
		os.Getenv("GEMINI_API_KEY"),
	)

	credJSON = strings.TrimSpace(
		os.Getenv("GOOGLE_CREDENTIALS_JSON"),
	)

	if sheetID == "" {
		log.Fatal(
			"GOOGLE_SHEET_ID est obligatoire",
		)
	}

	if geminiKey == "" {
		log.Fatal(
			"GEMINI_API_KEY est obligatoire",
		)
	}

	if credJSON == "" {
		log.Fatal(
			"GOOGLE_CREDENTIALS_JSON est obligatoire",
		)
	}

	ctx := context.Background()

	// ==================================================
	// GOOGLE SHEETS
	// ==================================================

	var err error

	sheetsSvc, err = sheets.NewService(
		ctx,
		option.WithCredentialsJSON(
			[]byte(credJSON),
		),
	)

	if err != nil {
		log.Fatalf(
			"Impossible d'initialiser Google Sheets: %v",
			err,
		)
	}

	log.Println("✓ Google Sheets connecté")

	// ==================================================
	// SQLITE / WHATSAPP STORE
	// ==================================================

	dbLog := waLog.Stdout(
		"Database",
		"WARN",
		true,
	)

	dbContainer, err := sqlstore.New(
		ctx,
		"sqlite3",
		"file:whatsapp.db?_foreign_keys=on",
		dbLog,
	)

	if err != nil {
		log.Fatalf(
			"Erreur initialisation SQLite: %v",
			err,
		)
	}

	log.Println("✓ Base WhatsApp SQLite initialisée")

	// ==================================================
	// DEVICE WHATSAPP
	// ==================================================

	deviceStore, err := dbContainer.GetFirstDevice(ctx)

	if err != nil {
		log.Fatalf(
			"Erreur récupération device WhatsApp: %v",
			err,
		)
	}

	// ==================================================
	// CLIENT WHATSAPP
	// ==================================================

	clientLog := waLog.Stdout(
		"WhatsApp",
		"WARN",
		true,
	)

	waClient = whatsmeow.NewClient(
		deviceStore,
		clientLog,
	)

	waClient.AddEventHandler(
		eventHandler,
	)

	// ==================================================
	// CONNEXION WHATSAPP
	// ==================================================

	if waClient.Store.ID == nil {

		log.Println(
			"Aucun compte WhatsApp connecté.",
		)

		log.Println(
			"Génération du QR Code...",
		)

		qrChan, err := waClient.GetQRChannel(ctx)

		if err != nil {
			log.Fatalf(
				"Erreur création canal QR: %v",
				err,
			)
		}

		err = waClient.Connect()

		if err != nil {
			log.Fatalf(
				"Erreur connexion WhatsApp: %v",
				err,
			)
		}

		go handleQR(qrChan)

	} else {

		log.Println(
			"Session WhatsApp existante trouvée.",
		)

		err = waClient.Connect()

		if err != nil {
			log.Fatalf(
				"Erreur reconnexion WhatsApp: %v",
				err,
			)
		}

		setLoggedIn(true)

		log.Println(
			"✓ WhatsApp reconnecté",
		)
	}

	// ==================================================
	// SERVEUR WEB
	// ==================================================

	http.HandleFunc(
		"/",
		serveDashboard,
	)

	http.HandleFunc(
		"/api/status",
		handleAPIStatus,
	)

	http.HandleFunc(
		"/api/toggle",
		handleAPIToggle,
	)

	port := os.Getenv("PORT")

	if port == "" {
		port = "8080"
	}

	log.Printf(
		"✓ Serveur démarré sur le port %s",
		port,
	)

	log.Fatal(
		http.ListenAndServe(
			":"+port,
			nil,
		),
	)
}

// ======================================================
// QR CODE
// ======================================================

func handleQR(
	qrChan <-chan whatsmeow.QRChannelItem,
) {
	for evt := range qrChan {

		switch evt.Event {

		case "code":

			png, err := qrcode.Encode(
				evt.Code,
				qrcode.Medium,
				256,
			)

			if err != nil {
				log.Printf(
					"Erreur génération QR: %v",
					err,
				)
				continue
			}

			qrMutex.Lock()

			currentQR =
				"data:image/png;base64," +
					base64.StdEncoding.EncodeToString(png)

			qrMutex.Unlock()

			setLoggedIn(false)

			log.Println(
				"✓ Nouveau QR Code généré",
			)

		case "success":

			qrMutex.Lock()
			currentQR = ""
			qrMutex.Unlock()

			setLoggedIn(true)

			log.Println(
				"✓ WhatsApp connecté avec succès",
			)

		case "timeout":

			qrMutex.Lock()
			currentQR = ""
			qrMutex.Unlock()

			setLoggedIn(false)

			log.Println(
				"QR Code expiré",
			)

		case "error":

			qrMutex.Lock()
			currentQR = ""
			qrMutex.Unlock()

			setLoggedIn(false)

			if evt.Error != nil {
				log.Printf(
					"Erreur QR WhatsApp: %v",
					evt.Error,
				)
			}

		default:

			log.Printf(
				"Événement QR WhatsApp: %s",
				evt.Event,
			)
		}
	}
}

// ======================================================
// ETAT CONNEXION
// ======================================================

func setLoggedIn(value bool) {

	statusMutex.Lock()
	isLoggedIn = value
	statusMutex.Unlock()
}

func getLoggedIn() bool {

	statusMutex.RLock()
	defer statusMutex.RUnlock()

	return isLoggedIn
}

// ======================================================
// GOOGLE SHEETS CONFIG
// ======================================================

func getConfig() (Config, error) {

	resp, err := sheetsSvc.
		Spreadsheets.
		Values.
		Get(
			sheetID,
			"Config!B1:B7",
		).
		Do()

	if err != nil {
		return Config{}, fmt.Errorf(
			"erreur lecture Config: %w",
			err,
		)
	}

	if len(resp.Values) < 7 {
		return Config{}, fmt.Errorf(
			"la feuille Config doit contenir 7 lignes minimum",
		)
	}

	adminValue := ""

	if len(resp.Values[0]) > 0 {
		adminValue = fmt.Sprintf(
			"%v",
			resp.Values[0][0],
		)
	}

	activeValue := ""

	if len(resp.Values[1]) > 0 {
		activeValue = fmt.Sprintf(
			"%v",
			resp.Values[1][0],
		)
	}

	limitValue := "0"

	if len(resp.Values[2]) > 0 {
		limitValue = fmt.Sprintf(
			"%v",
			resp.Values[2][0],
		)
	}

	consumedValue := "0"

	if len(resp.Values[3]) > 0 {
		consumedValue = fmt.Sprintf(
			"%v",
			resp.Values[3][0],
		)
	}

	promptValue := ""

	if len(resp.Values[6]) > 0 {
		promptValue = fmt.Sprintf(
			"%v",
			resp.Values[6][0],
		)
	}

	limite, err := strconv.ParseInt(
		strings.TrimSpace(limitValue),
		10,
		64,
	)

	if err != nil {
		limite = 0
	}

	consommes, err := strconv.ParseInt(
		strings.TrimSpace(consumedValue),
		10,
		64,
	)

	if err != nil {
		consommes = 0
	}

	return Config{
		AdminGlobal: strings.EqualFold(
			strings.TrimSpace(adminValue),
			"TRUE",
		),

		BotActif: strings.EqualFold(
			strings.TrimSpace(activeValue),
			"TRUE",
		),

		LimiteTokens: limite,

		TokensConsommes: consommes,

		PromptBusiness: promptValue,
	}, nil
}

// ======================================================
// EVENEMENTS WHATSAPP
// ======================================================

func eventHandler(evt interface{}) {

	messageEvent, ok := evt.(*events.Message)

	if !ok || messageEvent == nil {
		return
	}

	if messageEvent.Message == nil {
		return
	}

	// Ignorer nos propres messages
	if messageEvent.Info.IsFromMe {
		return
	}

	// ==================================================
	// EXTRAIRE MESSAGE
	// ==================================================

	msgText := extractMessageText(
		messageEvent,
	)

	if strings.TrimSpace(msgText) == "" {
		return
	}

	// ==================================================
	// CHAT JID
	// ==================================================

	chatJID := messageEvent.Info.Chat

	if chatJID.IsEmpty() {
		return
	}

	// ==================================================
	// CONFIGURATION
	// ==================================================

	cfg, err := getConfig()

	if err != nil {
		log.Printf(
			"Erreur lecture configuration: %v",
			err,
		)
		return
	}

	if !cfg.AdminGlobal {
		log.Println(
			"Message ignoré: AdminGlobal=false",
		)
		return
	}

	if !cfg.BotActif {
		log.Println(
			"Message ignoré: BotActif=false",
		)
		return
	}

	if cfg.LimiteTokens > 0 &&
		cfg.TokensConsommes >= cfg.LimiteTokens {

		log.Println(
			"Limite de tokens atteinte",
		)

		return
	}

	log.Printf(
		"Message reçu de %s: %s",
		chatJID.String(),
		msgText,
	)

	// ==================================================
	// TRAITEMENT AI
	// ==================================================

	go processAIResponse(
		chatJID,
		msgText,
		cfg,
	)
}

// ======================================================
// EXTRACTION TEXTE
// ======================================================

func extractMessageText(
	evt *events.Message,
) string {

	if evt == nil ||
		evt.Message == nil {
		return ""
	}

	// Message texte classique
	text := strings.TrimSpace(
		evt.Message.GetConversation(),
	)

	if text != "" {
		return text
	}

	// Message texte étendu
	extended :=
		evt.Message.GetExtendedTextMessage()

	if extended != nil {

		text = strings.TrimSpace(
			extended.GetText(),
		)

		if text != "" {
			return text
		}
	}

	return ""
}

// ======================================================
// GEMINI + REPONSE WHATSAPP
// ======================================================

func processAIResponse(
	chatJID types.JID,
	userMessage string,
	cfg Config,
) {

	// ==================================================
	// HISTORIQUE
	// ==================================================

	history := getHistory(
		chatJID.String(),
	)

	// ==================================================
	// MODELE GEMINI
	// ==================================================

	model := strings.TrimSpace(
		os.Getenv("GEMINI_MODEL"),
	)

	if model == "" {
		model = "gemini-2.5-flash"
	}

	apiURL := fmt.Sprintf(
		"https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent",
		model,
	)

	// ==================================================
	// TYPES GEMINI
	// ==================================================

	type Part struct {
		Text string `json:"text"`
	}

	type Content struct {
		Role  string `json:"role"`
		Parts []Part `json:"parts"`
	}

	contents := make(
		[]Content,
		0,
		len(history)+1,
	)

	// ==================================================
	// HISTORIQUE
	// ==================================================

	for _, item := range history {

		role := item["role"]

		if role != "user" &&
			role != "model" {
			continue
		}

		text := strings.TrimSpace(
			item["text"],
		)

		if text == "" {
			continue
		}

		contents = append(
			contents,
			Content{
				Role: role,
				Parts: []Part{
					{
						Text: text,
					},
				},
			},
		)
	}

	// ==================================================
	// NOUVEAU MESSAGE
	// ==================================================

	contents = append(
		contents,
		Content{
			Role: "user",
			Parts: []Part{
				{
					Text: userMessage,
				},
			},
		},
	)

	// ==================================================
	// REQUETE GEMINI
	// ==================================================

	requestBody := map[string]interface{}{
		"systemInstruction": map[string]interface{}{
			"parts": []map[string]string{
				{
					"text": cfg.PromptBusiness,
				},
			},
		},

		"contents": contents,
	}

	jsonBody, err := json.Marshal(
		requestBody,
	)

	if err != nil {
		log.Printf(
			"Erreur JSON Gemini: %v",
			err,
		)
		return
	}

	req, err := http.NewRequest(
		http.MethodPost,
		apiURL,
		bytes.NewReader(jsonBody),
	)

	if err != nil {
		log.Printf(
			"Erreur création requête Gemini: %v",
			err,
		)
		return
	}

	req.Header.Set(
		"Content-Type",
		"application/json",
	)

	req.Header.Set(
		"x-goog-api-key",
		geminiKey,
	)

	resp, err := httpClient.Do(req)

	if err != nil {
		log.Printf(
			"Erreur appel Gemini: %v",
			err,
		)
		return
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(
		resp.Body,
	)

	if err != nil {
		log.Printf(
			"Erreur lecture Gemini: %v",
			err,
		)
		return
	}

	// ==================================================
	// ERREUR HTTP
	// ==================================================

	if resp.StatusCode < 200 ||
		resp.StatusCode >= 300 {

		log.Printf(
			"Gemini HTTP %d: %s",
			resp.StatusCode,
			string(body),
		)

		return
	}

	// ==================================================
	// REPONSE GEMINI
	// ==================================================

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

	err = json.Unmarshal(
		body,
		&geminiResp,
	)

	if err != nil {
		log.Printf(
			"Erreur décodage réponse Gemini: %v",
			err,
		)
		return
	}

	if len(geminiResp.Candidates) == 0 {

		log.Printf(
			"Gemini n'a retourné aucun candidat: %s",
			string(body),
		)

		return
	}

	if len(
		geminiResp.Candidates[0].
			Content.
			Parts,
	) == 0 {

		log.Println(
			"Gemini a retourné une réponse sans texte",
		)

		return
	}

	aiReply := strings.TrimSpace(
		geminiResp.
			Candidates[0].
			Content.
			Parts[0].
			Text,
	)

	if aiReply == "" {
		log.Println(
			"Gemini a retourné une réponse vide",
		)
		return
	}

	tokensUsed :=
		geminiResp.
			UsageMetadata.
			TotalTokenCount

	// ==================================================
	// ENVOI WHATSAPP
	// ==================================================

	_, err = waClient.SendMessage(
		context.Background(),
		chatJID,
		&waE2E.Message{
			Conversation: proto.String(
				aiReply,
			),
		},
	)

	if err != nil {
		log.Printf(
			"Erreur envoi WhatsApp: %v",
			err,
		)
		return
	}

	log.Printf(
		"✓ Réponse envoyée à %s",
		chatJID.String(),
	)

	// ==================================================
	// SAUVEGARDE HISTORIQUE
	// ==================================================

	saveHistory(
		chatJID.String(),
		userMessage,
		aiReply,
	)

	// ==================================================
	// TOKENS
	// ==================================================

	if tokensUsed > 0 {
		updateTokens(tokensUsed)
	}
}

// ======================================================
// HISTORIQUE GOOGLE SHEETS
// ======================================================

func getHistory(
	sender string,
) []map[string]string {

	resp, err := sheetsSvc.
		Spreadsheets.
		Values.
		Get(
			sheetID,
			"History!A2:D",
		).
		Do()

	history := make(
		[]map[string]string,
		0,
	)

	if err != nil {
		log.Printf(
			"Erreur lecture History: %v",
			err,
		)

		return history
	}

	for _, row := range resp.Values {

		if len(row) < 4 {
			continue
		}

		rowSender :=
			fmt.Sprintf(
				"%v",
				row[1],
			)

		if rowSender != sender {
			continue
		}

		role :=
			fmt.Sprintf(
				"%v",
				row[2],
			)

		text :=
			fmt.Sprintf(
				"%v",
				row[3],
			)

		history = append(
			history,
			map[string]string{
				"role": role,
				"text": text,
			},
		)
	}

	return history
}

// ======================================================
// SAUVEGARDE HISTORIQUE
// ======================================================

func saveHistory(
	sender string,
	userMsg string,
	aiMsg string,
) {

	now := time.Now().
		Format(
			"2006-01-02 15:04:05",
		)

	values := [][]interface{}{
		{
			now,
			sender,
			"user",
			userMsg,
		},
		{
			now,
			sender,
			"model",
			aiMsg,
		},
	}

	vr := &sheets.ValueRange{
		Values: values,
	}

	_, err := sheetsSvc.
		Spreadsheets.
		Values.
		Append(
			sheetID,
			"History!A:D",
			vr,
		).
		ValueInputOption(
			"USER_ENTERED",
		).
		Do()

	if err != nil {
		log.Printf(
			"Erreur sauvegarde History: %v",
			err,
		)
	}
}

// ======================================================
// TOKENS
// ======================================================

func updateTokens(
	used int64,
) {

	if used <= 0 {
		return
	}

	tokenMutex.Lock()
	defer tokenMutex.Unlock()

	cfg, err := getConfig()

	if err != nil {
		log.Printf(
			"Erreur récupération tokens: %v",
			err,
		)
		return
	}

	newTotal :=
		cfg.TokensConsommes + used

	vr := &sheets.ValueRange{
		Values: [][]interface{}{
			{
				newTotal,
			},
		},
	}

	_, err = sheetsSvc.
		Spreadsheets.
		Values.
		Update(
			sheetID,
			"Config!B4",
			vr,
		).
		ValueInputOption(
			"USER_ENTERED",
		).
		Do()

	if err != nil {
		log.Printf(
			"Erreur mise à jour tokens: %v",
			err,
		)
		return
	}

	log.Printf(
		"✓ Tokens utilisés: +%d | Total: %d",
		used,
		newTotal,
	)
}

// ======================================================
// DASHBOARD
// ======================================================

func serveDashboard(
	w http.ResponseWriter,
	r *http.Request,
) {

	if r.URL.Path != "/" {
		http.NotFound(
			w,
			r,
		)
		return
	}

	http.ServeFile(
		w,
		r,
		"index.html",
	)
}

// ======================================================
// API STATUS
// ======================================================

func handleAPIStatus(
	w http.ResponseWriter,
	r *http.Request,
) {

	w.Header().Set(
		"Content-Type",
		"application/json; charset=utf-8",
	)

	cfg, err := getConfig()

	if err != nil {

		http.Error(
			w,
			`{"error":"Impossible de lire la configuration"}`,
			http.StatusInternalServerError,
		)

		return
	}

	qrMutex.RLock()
	qr := currentQR
	qrMutex.RUnlock()

	response := map[string]interface{}{
		"admin":     cfg.AdminGlobal,
		"actif":     cfg.BotActif,
		"limite":    cfg.LimiteTokens,
		"consommes": cfg.TokensConsommes,
		"connected": getLoggedIn(),
		"qr":        qr,
	}

	err = json.NewEncoder(w).
		Encode(response)

	if err != nil {
		log.Printf(
			"Erreur réponse API status: %v",
			err,
		)
	}
}

// ======================================================
// API TOGGLE
// ======================================================

func handleAPIToggle(
	w http.ResponseWriter,
	r *http.Request,
) {

	w.Header().Set(
		"Content-Type",
		"application/json; charset=utf-8",
	)

	cfg, err := getConfig()

	if err != nil {

		http.Error(
			w,
			`{"error":"Configuration inaccessible"}`,
			http.StatusInternalServerError,
		)

		return
	}

	if !cfg.AdminGlobal {

		http.Error(
			w,
			`{"error":"Opération non autorisée par l'Admin"}`,
			http.StatusForbidden,
		)

		return
	}

	nouvelEtat := !cfg.BotActif

	vr := &sheets.ValueRange{
		Values: [][]interface{}{
			{
				nouvelEtat,
			},
		},
	}

	_, err = sheetsSvc.
		Spreadsheets.
		Values.
		Update(
			sheetID,
			"Config!B2",
			vr,
		).
		ValueInputOption(
			"USER_ENTERED",
		).
		Do()

	if err != nil {

		log.Printf(
			"Erreur toggle bot: %v",
			err,
		)

		http.Error(
			w,
			`{"error":"Impossible de modifier le statut du bot"}`,
			http.StatusInternalServerError,
		)

		return
	}

	err = json.NewEncoder(w).
		Encode(
			map[string]interface{}{
				"status": "ok",
				"actif":  nouvelEtat,
			},
		)

	if err != nil {
		log.Printf(
			"Erreur réponse toggle: %v",
			err,
		)
	}
}
