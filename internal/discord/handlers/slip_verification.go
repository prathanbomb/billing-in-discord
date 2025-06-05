package handlers

import (
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/oatsaysai/billing-in-discord/internal/db"
	"github.com/oatsaysai/billing-in-discord/pkg/ocr"
	"github.com/oatsaysai/billing-in-discord/pkg/verifier"
)

var (
	verifierClient *verifier.Client
)

// SetVerifierClient sets the verifier client
func SetVerifierClient(client *verifier.Client) {
	verifierClient = client
}

// HandleSlipVerification handles slip verification replies
func HandleSlipVerification(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.MessageReference == nil || m.MessageReference.MessageID == "" || len(m.Attachments) == 0 {
		return
	}

	if verifierClient == nil {
		SendErrorMessage(s, m.ChannelID, "Slip verification service is not configured. Slip verification is not available.")
		return
	}

	refMsg, err := s.ChannelMessage(m.ChannelID, m.MessageReference.MessageID)
	if err != nil {
		log.Printf("SlipVerify: Error fetching referenced message %s: %v", m.MessageReference.MessageID, err)
		return
	}

	// Ensure the referenced message is from the bot itself
	if refMsg.Author == nil || refMsg.Author.ID != s.State.User.ID {
		return
	}

	debtorDiscordID, amount, txIDs, err := db.ParseBotQRMessageContent(refMsg.Content)
	if err != nil {
		log.Printf("SlipVerify: Could not parse bot message content: %v", err)
		// Don't send error to user, might be a reply to a non-QR bot message
		return
	}

	log.Printf("SlipVerify: Received slip verification for debtor %s, amount %.2f, TxIDs %v", debtorDiscordID, amount, txIDs)
	slipUploaderID := m.Author.ID
	var slipURL string

	for _, att := range m.Attachments {
		if strings.HasPrefix(strings.ToLower(att.ContentType), "image/") {
			slipURL = att.URL
			break
		}
	}

	if slipURL == "" {
		return // No image attachment found
	}

	// The person uploading the slip should be the one mentioned as the debtor in the QR message
	if slipUploaderID != debtorDiscordID {
		log.Printf("SlipVerify: Slip uploaded by %s for debtor %s - ignoring (uploader mismatch).", slipUploaderID, debtorDiscordID)
		return
	}

	tmpFile := fmt.Sprintf("slip_%s_%s.png", m.ID, debtorDiscordID) // Unique temp file name
	err = ocr.DownloadFile(tmpFile, slipURL)
	if err != nil {
		SendErrorMessage(s, m.ChannelID, "ไม่สามารถดาวน์โหลดรูปภาพสลิปเพื่อยืนยันได้")
		log.Printf("SlipVerify: Failed to download slip %s: %v", slipURL, err)
		return
	}
	defer os.Remove(tmpFile)

	verifyResp, err := verifierClient.VerifySlip(amount, tmpFile)
	if err != nil {
		SendErrorMessage(s, m.ChannelID, fmt.Sprintf("การเรียก API ยืนยันสลิปล้มเหลว: %v", err))
		log.Printf("SlipVerify: API call failed for debtor %s, amount %.2f: %v", debtorDiscordID, amount, err)
		return
	}

	// Check if amount from slip matches expected amount (with tolerance)
	if !(verifyResp.Data.Amount > amount-0.01 && verifyResp.Data.Amount < amount+0.01) {
		SendErrorMessage(s, m.ChannelID, fmt.Sprintf("จำนวนเงินในสลิป (%.2f) ไม่ตรงกับจำนวนที่คาดไว้ (%.2f)", verifyResp.Data.Amount, amount))
		return
	}

	intendedPayeeDiscordID := "???" // Placeholder
	// Try to determine intended payee
	if len(txIDs) > 0 {
		// If TxIDs are present, payee is determined from the first TxID
		payeeDbID, fetchErr := db.GetPayeeDbIDFromTx(txIDs[0])
		if fetchErr == nil {
			payeeDiscordID, _ := db.GetDiscordIDFromDbID(payeeDbID)
			intendedPayeeDiscordID = payeeDiscordID
		}
	} else {
		// If no TxIDs, try to find payee based on debtor and amount
		payee, findErr := db.FindIntendedPayee(debtorDiscordID, amount)
		if findErr != nil {
			SendErrorMessage(s, m.ChannelID, fmt.Sprintf("ไม่สามารถระบุผู้รับเงินที่ถูกต้องสำหรับการชำระเงินนี้ได้: %v", findErr))
			log.Printf("SlipVerify: Could not determine intended payee for debtor %s, amount %.2f: %v", debtorDiscordID, amount, findErr)
			return
		}
		intendedPayeeDiscordID = payee
	}

	if intendedPayeeDiscordID == "???" || intendedPayeeDiscordID == "" {
		log.Printf("SlipVerify: Critical - Failed to determine intended payee for debtor %s, amount %.2f", debtorDiscordID, amount)
		SendErrorMessage(s, m.ChannelID, "เกิดข้อผิดพลาดร้ายแรง: ไม่สามารถระบุผู้รับเงินได้")
		return
	}

	// Process payment based on TxIDs if available
	if len(txIDs) > 0 {
		log.Printf("SlipVerify: Attempting batch update using TxIDs: %v", txIDs)
		successCount := 0
		failCount := 0
		var failMessages []string

		for _, txID := range txIDs {
			err = db.MarkTransactionPaidAndUpdateDebt(txID) // This function handles both transaction and user_debt updates
			if err == nil {
				successCount++

				// Check and send automatic praise if applicable
				CheckAndSendAutomaticPraise(s, m.ChannelID, txID, debtorDiscordID)
			} else {
				failCount++
				failMessages = append(failMessages, fmt.Sprintf("TxID %d (%v)", txID, err))
				log.Printf("SlipVerify: Failed update for TxID %d: %v", txID, err)
			}
		}

		var report strings.Builder
		report.WriteString(fmt.Sprintf(
			"✅ สลิปได้รับการยืนยัน!\n- ผู้จ่าย: <@%s>\n- ผู้รับ: <@%s>\n- จำนวน: %.2f บาท\n- ผู้ส่ง (สลิป): %s (%s)\n- ผู้รับ (สลิป): %s (%s)\n- วันที่ (สลิป): %s\n- เลขอ้างอิง (สลิป): %s\n",
			debtorDiscordID, intendedPayeeDiscordID, amount,
			verifyResp.Data.SenderName, verifyResp.Data.SenderID,
			verifyResp.Data.ReceiverName, verifyResp.Data.ReceiverID,
			verifyResp.Data.Date, verifyResp.Data.Ref,
		))
		report.WriteString(fmt.Sprintf("อัปเดตสำเร็จ %d/%d รายการธุรกรรม (TxIDs: %v)\n", successCount, len(txIDs), txIDs))
		if failCount > 0 {
			report.WriteString(fmt.Sprintf("⚠️ เกิดข้อผิดพลาด %d รายการ: %s", failCount, strings.Join(failMessages, "; ")))
		}
		s.ChannelMessageSend(m.ChannelID, report.String())
		return

	} else { // No TxIDs, general debt reduction
		log.Printf("SlipVerify: No TxIDs found in message. Attempting general debt reduction for %s paying %s amount %.2f.", debtorDiscordID, intendedPayeeDiscordID, amount)

		errReduce := db.ReduceDebtFromPayment(debtorDiscordID, intendedPayeeDiscordID, amount)
		if errReduce != nil {
			SendErrorMessage(s, m.ChannelID, fmt.Sprintf("เกิดข้อผิดพลาดในการลดหนี้สินทั่วไปสำหรับ <@%s> ถึง <@%s>: %v", debtorDiscordID, intendedPayeeDiscordID, errReduce))
			log.Printf("SlipVerify: Failed general debt reduction for %s to %s (%.2f): %v", debtorDiscordID, intendedPayeeDiscordID, amount, errReduce)
			return
		}
		CheckAndAwardBadges(s, debtorDiscordID, m.ChannelID)
		CheckAndAwardBadges(s, intendedPayeeDiscordID, m.ChannelID)
		s.ChannelMessageSend(m.ChannelID, fmt.Sprintf(
			"✅ สลิปได้รับการยืนยัน & ยอดหนี้สินจาก <@%s> ถึง <@%s> ลดลง %.2f บาท!\n- ผู้ส่ง (สลิป): %s (%s)\n- ผู้รับ (สลิป): %s (%s)\n- วันที่ (สลิป): %s\n- เลขอ้างอิง (สลิป): %s",
			debtorDiscordID, intendedPayeeDiscordID, amount,
			verifyResp.Data.SenderName, verifyResp.Data.SenderID,
			verifyResp.Data.ReceiverName, verifyResp.Data.ReceiverID,
			verifyResp.Data.Date, verifyResp.Data.Ref,
		))
	}
}
