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
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	waE2E "go.mau.fi/whatsmeow/proto/waE2E"

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
	log.Println("========================================")
	log.Println("   WHATSAPP AI BOT - DEMARRAGE")
	log.Println("========================================")

	// --------------------------------------------------
	// Variables d'environnement
	// --------------------------------------------------

	sheetID = strings.TrimSpace(os.Getenv("GOOGLE_SHEET_ID"))
	geminiKey = strings.TrimSpace(os.Getenv("GEMINI_API_KEY"))
	credJSON := strings.TrimSpace(os.Getenv("GOOGLE_CREDENTIALS_JSON"))

	if sheetID == "" {
		log.Fatal("GOOGLE_SHEET_ID est obligatoire")
	}

	if geminiKey == "" {
		log.Fatal("GEMINI_API_KEY est obligatoire")
	}

	if credJSON == "" {
		log.Fatal("GOOGLE_CREDENTIALS_JSON est obligatoire")
	}

	ctx := context.Background()

	// --------------------------------------------------
	// Google Sheets
	// --------------------------------------------------

	var err error

	sheetsSvc, err = sheets.NewService(
		ctx,
		option.WithCredentialsJSON([]byte(credJSON)),
	)

	if err != nil {
		log.Fatalf(
			"Impossible d'initialiser Google Sheets: %v",
			err,
		)
	}

	log.Println("✓ Google Sheets connecté")

	// --------------------------------------------------
	// Base SQLite WhatsApp
	// --------------------------------------------------

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

	log.Println("✓ Base SQLite initialisée")

	// --------------------------------------------------
	// Récupération du device WhatsApp
	// --------------------------------------------------

	deviceStore, err := dbContainer.GetFirstDevice(ctx)

	if err != nil {
		log.Fatalf(
			"Erreur récupération device WhatsApp: %v",
			err,
		)
	}

	// --------------------------------------------------
	// Client WhatsApp
	// --------------------------------------------------

	clientLog := waLog.Stdout(
		"WhatsApp",
		"WARN",
		true,
	)

	waClient = whatsmeow.NewClient(
		deviceStore,
		clientLog,
	)

	waClient.AddEventHandler(eventHandler)

	// --------------------------------------------------
	// Connexion / QR Code
	// --------------------------------------------------

	if waClient.Store.ID == nil {

		log.Println("Aucun compte WhatsApp associé.")
		log.Println("Génération du QR Code...")

		qrChan, err := waClient.GetQRChannel(ctx)

		if err != nil {
			log.Fatalf(
				"Impossible de créer le canal QR: %v",
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
			"Session WhatsApp existante détectée.",
		)

		err = waClient.Connect()

		if err != nil {
			log.Fatalf(
				"Erreur reconnexion WhatsApp: %v",
				err,
			)
		}

		setLoggedIn(true)

		log.Println("✓ WhatsApp reconnecté")
	}

	// --------------------------------------------------
	// Serveur Web
	// --------------------------------------------------

	http.HandleFunc("/", serveDashboard)
	http.HandleFunc("/api/status", handleAPIStatus)
	http.HandleFunc("/api/toggle", handleAPIToggle)

	port := os.Getenv("PORT")

	if port == "" {
		port = "8080"
	}

	log.Printf(
		"✓ Serveur HTTP démarré sur le port %s",
		port,
	)

	log.Fatal(
		http.ListenAndServe(":"+port, nil),
	)
}

// ======================================================
// QR CODE WHATSAPP
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

			log.Println("✓ Nouveau QR Code disponible")

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
				"QR Code expiré.",
			)

		default:

			log.Printf(
				"Événement WhatsApp: %s",
				evt.Event,
			)
		}
	}
}

// ======================================================
// ETAT DE CONNEXION
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
// CONFIGURATION GOOGLE SHEETS
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
			"la feuille Config doit contenir au moins 7 lignes en colonne B",
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

	limitValue := ""

	if len(resp.Values[2]) > 0 {
		limitValue = fmt.Sprintf(
			"%v",
			resp.Values[2][0],
		)
	}

	consumedValue := ""

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
// RECEPTION DES MESSAGES WHATSAPP
// ======================================================

func eventHandler(evt interface{}) {

	messageEvent, ok := evt.(*events.Message)

	if !ok {
		return
	}

	if messageEvent == nil ||
		messageEvent.Message == nil {
		return
	}

	// Ignorer nos propres messages
	if messageEvent.Info.IsFromMe {
		return
	}

	// --------------------------------------------------
	// Récupérer le texte
	// --------------------------------------------------

	msgText := extractMessageText(
		messageEvent,
	)

	if strings.TrimSpace(msgText) == "" {
		return
	}

	// --------------------------------------------------
	// JID du chat
	// --------------------------------------------------

	chatJID := messageEvent.Info.Chat

	if chatJID.IsEmpty() {
		return
	}

	// --------------------------------------------------
	// Configuration
	// --------------------------------------------------

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
			"Message ignoré: AdminGlobal désactivé.",
		)
		return
	}

	if !cfg.BotActif {
		log.Println(
			"Message ignoré: BotActif désactivé.",
		)
		return
	}

	if cfg.LimiteTokens > 0 &&
		cfg.TokensConsommes >= cfg.LimiteTokens {

		log.Println(
			"Limite de tokens atteinte.",
		)

		return
	}

	log.Printf(
		"Message reçu de %s: %s",
		chatJID.String(),
		msgText,
	)

	// --------------------------------------------------
	// Traitement asynchrone
	// --------------------------------------------------

	go processAIResponse(
		chatJID,
		msgText,
		cfg,
	)
}

// ======================================================
// EXTRACTION DU MESSAGE
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
	extended := evt.Message.GetExtendedTextMessage()

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
// GEMINI
// ======================================================

func processAIResponse(
	chatJID interface {
		String() string
	},
	userMessage string,
	cfg Config,
) {

	// Cette fonction reçoit un type générique pour éviter
	// de mélanger la logique Gemini avec WhatsApp.
	// Le vrai JID est récupéré ci-dessous.

	jidString := chatJID.String()

	// --------------------------------------------------
	// Récupérer l'historique
	// --------------------------------------------------

	history := getHistory(
		jidString,
	)

	// --------------------------------------------------
	// Modèle Gemini
	// --------------------------------------------------

	model := os.Getenv(
		"GEMINI_MODEL",
	)

	if model == "" {
		model = "gemini-2.5-flash"
	}

	apiURL := fmt.Sprintf(
		"https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent",
		model,
	)

	// --------------------------------------------------
	// Construction de la conversation
	// --------------------------------------------------

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

	// Historique
	for _, h := range history {

		role := h["role"]

		if role != "user" &&
			role != "model" {
			continue
		}

		text := strings.TrimSpace(
			h["text"],
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

	// Nouveau message utilisateur
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

	// --------------------------------------------------
	// Instruction système
	// --------------------------------------------------

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

	// --------------------------------------------------
	// Requête Gemini
	// --------------------------------------------------

	req, err := http.NewRequest(
		http.MethodPost,
		apiURL,
		bytes.NewBuffer(jsonBody),
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

	if resp.StatusCode < 200 ||
		resp.StatusCode >= 300 {

		log.Printf(
			"Gemini HTTP %d: %s",
			resp.StatusCode,
			string(body),
		)

		return
	}

	// --------------------------------------------------
	// Réponse Gemini
	// --------------------------------------------------

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

		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.Unmarshal(
		body,
		&geminiResp,
	); err != nil {

		log.Printf(
			"Erreur décodage réponse Gemini: %v",
			err,
		)

		return
	}

	if len(geminiResp.Candidates) == 0 {

		log.Printf(
			"Gemini n'a retourné aucune réponse: %s",
			string(body),
		)

		return
	}

	if len(
		geminiResp.Candidates[0].Content.Parts,
	) == 0 {

		log.Println(
			"Gemini a retourné un candidat sans texte.",
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
			"Réponse Gemini vide.",
		)
		return
	}

	tokensUsed :=
		geminiResp.
			UsageMetadata.
			TotalTokenCount

	// --------------------------------------------------
	// Envoyer la réponse au BON chat WhatsApp
	// --------------------------------------------------

	// Reconvertir le JID string en JID WhatsApp
	targetJID, err := parseJID(jidString)

	if err != nil {
		log.Printf(
			"JID WhatsApp invalide %s: %v",
			jidString,
			err,
		)
		return
	}

	_, err = waClient.SendMessage(
		context.Background(),
		targetJID,
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
		jidString,
	)

	// --------------------------------------------------
	// Sauvegarde historique
	// --------------------------------------------------

	saveHistory(
		jidString,
		userMessage,
		aiReply,
	)

	// --------------------------------------------------
	// Mise à jour des tokens
	// --------------------------------------------------

	if tokensUsed > 0 {
		updateTokens(
			tokensUsed,
		)
	}
}

// ======================================================
// JID
// ======================================================

func parseJID(
	value string,
) (interface {
	String() string
}, error) {

	// Cette fonction est remplacée directement par la version
	// typée ci-dessous.
	return parseWhatsAppJID(value)
}
