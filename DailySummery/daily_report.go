package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/smtp"
	"os"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/joho/godotenv"
)

type DailyData struct {
	ReportDate          string
	DailySales          string
	TransactionCount    string
	AverageSale         string
	HCCTransactions     string
	ScanRate            string
	RoundUpTransactions string
	RoundUpRate         string
}

func main() {
	// 1. Load Environment Variables
	err := godotenv.Load()
	if err != nil {
		log.Println("Warning: No .env file found, relying on system environment variables")
	}

	// 2. Parse Command Line Flags
	// Defaults to today's date if no flag is provided
	defaultDate := time.Now().Format("2006-01-02")
	targetDateStr := flag.String("date", defaultDate, "Target date for the daily report (YYYY-MM-DD)")
	recipientFlag := flag.String("recipient", "", "Single email address to send to (overrides recipients.txt)")
	flag.Parse()

	// 3. Connect to Database
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s",
		os.Getenv("DB_USER"),
		os.Getenv("DB_PASS"),
		os.Getenv("DB_HOST"),
		os.Getenv("DB_PORT"),
		os.Getenv("DB_NAME"),
	)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		log.Fatalf("Error connecting to database: %v", err)
	}
	defer db.Close()

	// 4. Run the Daily Query
	// Uses a subquery to find transactions with the round-up item without causing duplicate sales rows
	query := `
		SELECT 
			COALESCE(SUM(x.dwsx_invoice_total), 0) AS Daily_Sales,
			COUNT(x.dwsx_transaction) AS Transaction_Count,
			SUM(CASE WHEN x.dwsx_hhc_id_number IS NOT NULL AND TRIM(x.dwsx_hhc_id_number) != '' THEN 1 ELSE 0 END) AS HCC_Transactions,
			SUM(CASE WHEN r.round_up_trx IS NOT NULL THEN 1 ELSE 0 END) AS Round_Up_Transactions
		FROM EAGLEDW.dw_sls_xaction_dtl x
		LEFT JOIN (
			SELECT DISTINCT dwsi_store, dwsi_terminal, dwsi_transaction, dwsi_transaction_date, 1 AS round_up_trx
			FROM EAGLEDW.dw_sls_item_dtl
			WHERE dwsi_transaction_date = ? AND dwsi_item = 9269862
		) r ON x.dwsx_store = r.dwsi_store 
			AND x.dwsx_terminal = r.dwsi_terminal 
			AND x.dwsx_transaction = r.dwsi_transaction 
			AND x.dwsx_transaction_date = r.dwsi_transaction_date
		WHERE x.dwsx_transaction_date = ?;`

	var dSales sql.NullFloat64
	var tCount, hccCount, roundUpCount sql.NullInt64

	// Pass the target date twice (once for the subquery, once for the main WHERE clause)
	err = db.QueryRow(query, *targetDateStr, *targetDateStr).Scan(&dSales, &tCount, &hccCount, &roundUpCount)
	if err != nil && err != sql.ErrNoRows {
		log.Fatalf("Daily Query failed: %v", err)
	}

	// 5. Calculate Rates and Averages
	avgSale, scanRate, roundUpRate := 0.0, 0.0, 0.0

	if tCount.Int64 > 0 {
		avgSale = dSales.Float64 / float64(tCount.Int64)
		scanRate = (float64(hccCount.Int64) / float64(tCount.Int64)) * 100
		roundUpRate = (float64(roundUpCount.Int64) / float64(tCount.Int64)) * 100
	}

	// 6. Build Data Struct for Template
	data := DailyData{
		ReportDate:          *targetDateStr,
		DailySales:          fmt.Sprintf("%.2f", dSales.Float64),
		TransactionCount:    fmt.Sprintf("%d", tCount.Int64),
		AverageSale:         fmt.Sprintf("%.2f", avgSale),
		HCCTransactions:     fmt.Sprintf("%d", hccCount.Int64),
		ScanRate:            fmt.Sprintf("%.1f", scanRate),
		RoundUpTransactions: fmt.Sprintf("%d", roundUpCount.Int64),
		RoundUpRate:         fmt.Sprintf("%.1f", roundUpRate),
	}

	// 7. Parse & Execute Template
	tmpl, err := template.ParseFiles("daily_template.html")
	if err != nil {
		log.Fatalf("Error parsing template: %v", err)
	}

	var htmlBody bytes.Buffer
	if err := tmpl.Execute(&htmlBody, data); err != nil {
		log.Fatalf("Error executing template: %v", err)
	}

	// 8. Determine Recipients
	var recipients []string

	if *recipientFlag != "" {
		recipients = append(recipients, *recipientFlag)
		fmt.Printf("Using command-line recipient, ignoring recipients.txt: %s\n", *recipientFlag)
	} else {
		file, err := os.Open("recipients.txt")
		if err != nil {
			log.Fatalf("Error reading recipients.txt: %v", err)
		}
		defer file.Close()

		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			email := strings.TrimSpace(scanner.Text())
			if email != "" {
				recipients = append(recipients, email)
			}
		}
	}

	if len(recipients) == 0 {
		log.Fatalf("No recipients provided. Use -recipient flag or add emails to recipients.txt")
	}

	// 9. Send Email via SMTP2GO
	smtpHost := os.Getenv("SMTP2GO_HOST")
	smtpPort := os.Getenv("SMTP2GO_PORT")
	smtpUser := os.Getenv("SMTP2GO_USER")
	smtpPass := os.Getenv("SMTP2GO_PASS")
	senderEmail := os.Getenv("SENDER_EMAIL")

	auth := smtp.PlainAuth("", smtpUser, smtpPass, smtpHost)

	// Construct MIME headers for HTML email
	subject := fmt.Sprintf("Subject: Daily Performance Report - %s\r\n", *targetDateStr)
	toHeader := fmt.Sprintf("To: %s\r\n", strings.Join(recipients, ","))
	fromHeader := fmt.Sprintf("From: Compass Reports <%s>\r\n", senderEmail)
	mime := "MIME-version: 1.0;\nContent-Type: text/html; charset=\"UTF-8\";\n\n"

	msg := []byte(fromHeader + toHeader + subject + mime + htmlBody.String())

	addr := fmt.Sprintf("%s:%s", smtpHost, smtpPort)
	err = smtp.SendMail(addr, auth, senderEmail, recipients, msg)
	if err != nil {
		log.Fatalf("Error sending email via SMTP2GO: %v", err)
	}

	fmt.Println("Daily Report sent successfully via SMTP2GO!")
}
