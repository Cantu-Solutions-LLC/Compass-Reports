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

type DailyProjection struct {
	ProjectedDate           string
	ProjectedNetSales       string
	ProjectedCustomerCount  string
	ProjectedAvgTransaction string
	DailyGoalPct            string
}

type EmailData struct {
	CurrentDate               string
	TargetDate                string
	IncreasePct               string
	CurrentMTDNetSales        string
	ProjectedLYMTD            string
	ProjectedLYFullMonth      string
	PctToLYMTDGoal            string
	PctToLYFullMonthGoal      string
	DailyProjections          []DailyProjection
	TotalWeeklyProjectedSales string
	TotalWeeklyCustomers      string
	TotalWeeklyAvgTransaction string
	TotalWeeklyGoalPct        string
}

func main() {
	// 1. Load Environment Variables
	err := godotenv.Load()
	if err != nil {
		log.Println("Warning: No .env file found, relying on system environment variables")
	}

	// 2. Parse Command Line Flags
	defaultDate := time.Now().Format("2006-01-02")
	targetDateStr := flag.String("date", defaultDate, "Target date for the weekly report (YYYY-MM-DD)")
	increasePct := flag.Float64("increase", 0.0, "Projected increase percentage (e.g. 8 for 8%)")
	recipientFlag := flag.String("recipient", "", "Single email address to send to (overrides recipients.txt)")
	flag.Parse()

	multiplier := 1.0 + (*increasePct / 100.0)

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

	// 4. Run MTD Query
	mtdQuery := `
		SELECT 
			SUM(CASE WHEN dwsx_transaction_date >= DATE_FORMAT(CURDATE(), '%Y-%m-01') AND dwsx_transaction_date <= CURDATE() THEN dwsx_invoice_total ELSE 0 END) AS Current_MTD,
			SUM(CASE WHEN dwsx_transaction_date >= DATE_FORMAT(DATE_SUB(CURDATE(), INTERVAL 1 YEAR), '%Y-%m-01') AND dwsx_transaction_date <= DATE_SUB(CURDATE(), INTERVAL 1 YEAR) THEN dwsx_invoice_total ELSE 0 END) * ? AS Projected_LY_MTD,
			SUM(CASE WHEN dwsx_transaction_date >= DATE_FORMAT(DATE_SUB(CURDATE(), INTERVAL 1 YEAR), '%Y-%m-01') AND dwsx_transaction_date <= LAST_DAY(DATE_SUB(CURDATE(), INTERVAL 1 YEAR)) THEN dwsx_invoice_total ELSE 0 END) * ? AS Projected_LY_Full
		FROM EAGLEDW.dw_sls_xaction_dtl
		WHERE (dwsx_transaction_date >= DATE_FORMAT(CURDATE(), '%Y-%m-01') AND dwsx_transaction_date <= CURDATE())
		   OR (dwsx_transaction_date >= DATE_FORMAT(DATE_SUB(CURDATE(), INTERVAL 1 YEAR), '%Y-%m-01') AND dwsx_transaction_date <= LAST_DAY(DATE_SUB(CURDATE(), INTERVAL 1 YEAR)));`

	var currentMTD, projectedLYMTD, projectedLYFull sql.NullFloat64
	err = db.QueryRow(mtdQuery, multiplier, multiplier).Scan(&currentMTD, &projectedLYMTD, &projectedLYFull)
	if err != nil && err != sql.ErrNoRows {
		log.Fatalf("MTD Query failed: %v", err)
	}

	pctToLYMTDGoal, pctToLYFullMonthGoal := 0.0, 0.0
	if projectedLYMTD.Float64 > 0 {
		pctToLYMTDGoal = (currentMTD.Float64 / projectedLYMTD.Float64) * 100
	}
	if projectedLYFull.Float64 > 0 {
		pctToLYFullMonthGoal = (currentMTD.Float64 / projectedLYFull.Float64) * 100
	}

	// 5. Run Weekly Prediction Query
	weeklyQuery := `
		SELECT 
			DATE_FORMAT(DATE_ADD(dwsx_transaction_date, INTERVAL 52 WEEK), '%Y-%m-%d') AS Projected_Date,
			SUM(dwsx_invoice_total) * ? AS Projected_Net_Sales,
			COUNT(DISTINCT dwsx_transaction) * ? AS Projected_Customer_Count,
			SUM(dwsx_invoice_total) / COUNT(DISTINCT dwsx_transaction) AS Projected_Avg_Transaction
		FROM EAGLEDW.dw_sls_xaction_dtl
		WHERE dwsx_transaction_date >= DATE_SUB(?, INTERVAL 52 WEEK)
		  AND dwsx_transaction_date < DATE_ADD(DATE_SUB(?, INTERVAL 52 WEEK), INTERVAL 7 DAY)
		GROUP BY dwsx_transaction_date
		ORDER BY dwsx_transaction_date ASC;`

	rows, err := db.Query(weeklyQuery, multiplier, multiplier, *targetDateStr, *targetDateStr)
	if err != nil {
		log.Fatalf("Weekly Query failed: %v", err)
	}
	defer rows.Close()

	var dailyProjections []DailyProjection
	var totalSales, totalCount, totalAvg, actualWeeklySales float64

	for rows.Next() {
		var pDate sql.NullString
		var pSales, pCount, pAvg sql.NullFloat64

		if err := rows.Scan(&pDate, &pSales, &pCount, &pAvg); err != nil {
			log.Fatalf("Row scan failed: %v", err)
		}

		actualDailySales := 0.0
		dailyGoalPct := 0.0
		if pSales.Float64 > 0 {
			dailyGoalPct = (actualDailySales / pSales.Float64) * 100
		}

		dailyProjections = append(dailyProjections, DailyProjection{
			ProjectedDate:           pDate.String,
			ProjectedNetSales:       fmt.Sprintf("%.2f", pSales.Float64),
			ProjectedCustomerCount:  fmt.Sprintf("%.0f", pCount.Float64),
			ProjectedAvgTransaction: fmt.Sprintf("%.2f", pAvg.Float64),
			DailyGoalPct:            fmt.Sprintf("%.1f", dailyGoalPct),
		})

		totalSales += pSales.Float64
		totalCount += pCount.Float64
		actualWeeklySales += actualDailySales
	}

	if totalCount > 0 {
		totalAvg = totalSales / totalCount
	}

	weeklyGoalPct := 0.0
	if totalSales > 0 {
		weeklyGoalPct = (actualWeeklySales / totalSales) * 100
	}

	// 6. Build Data Struct
	data := EmailData{
		CurrentDate:               defaultDate,
		TargetDate:                *targetDateStr,
		IncreasePct:               fmt.Sprintf("%g", *increasePct),
		CurrentMTDNetSales:        fmt.Sprintf("%.2f", currentMTD.Float64),
		ProjectedLYMTD:            fmt.Sprintf("%.2f", projectedLYMTD.Float64),
		ProjectedLYFullMonth:      fmt.Sprintf("%.2f", projectedLYFull.Float64),
		PctToLYMTDGoal:            fmt.Sprintf("%.1f", pctToLYMTDGoal),
		PctToLYFullMonthGoal:      fmt.Sprintf("%.1f", pctToLYFullMonthGoal),
		DailyProjections:          dailyProjections,
		TotalWeeklyProjectedSales: fmt.Sprintf("%.2f", totalSales),
		TotalWeeklyCustomers:      fmt.Sprintf("%.0f", totalCount),
		TotalWeeklyAvgTransaction: fmt.Sprintf("%.2f", totalAvg),
		TotalWeeklyGoalPct:        fmt.Sprintf("%.1f", weeklyGoalPct),
	}

	// 7. Parse & Execute Template
	tmpl, err := template.ParseFiles("template.html")
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
	subject := fmt.Sprintf("Subject: Weekly Sales Projection (+%g%%) & MTD Report\r\n", *increasePct)
	toHeader := fmt.Sprintf("To: %s\r\n", strings.Join(recipients, ","))
	fromHeader := fmt.Sprintf("From: Compass Reports <%s>\r\n", senderEmail)
	mime := "MIME-version: 1.0;\nContent-Type: text/html; charset=\"UTF-8\";\n\n"

	msg := []byte(fromHeader + toHeader + subject + mime + htmlBody.String())

	addr := fmt.Sprintf("%s:%s", smtpHost, smtpPort)
	err = smtp.SendMail(addr, auth, senderEmail, recipients, msg)
	if err != nil {
		log.Fatalf("Error sending email via SMTP2GO: %v", err)
	}

	fmt.Println("Email sent successfully via SMTP2GO!")
}
