package finance

// Staff pages of signature by link (specs/049-contract-esign). No page ever shows a link.

import (
	"io"
	"net/mail"
	"strconv"
	"strings"
)

// signerLines reads "Full Name <address>" lines.
func signerLines(field, text, role string) ([]SignerInput, error) {
	var out []SignerInput
	for i, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		a, err := mail.ParseAddress(line)
		if err != nil || a.Name == "" {
			return nil, invalid(field, "line %d: write it as Full Name <address@example.com>", i+1)
		}
		out = append(out, SignerInput{Name: a.Name, Email: a.Address, Role: role})
	}
	return out, nil
}

func (u *UI) signingSend(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	im, err := ifMatchForm(c.r)
	if err != nil {
		return err
	}
	in := SigningRequest{Ordered: c.form("ordered") == "yes"}
	for _, f := range [][2]string{{"counterparty", RoleCounterparty}, {"company", RoleCompany}} {
		list, err := signerLines(f[0], c.r.PostForm.Get(f[0]), f[1])
		if err != nil {
			return err
		}
		in.Signers = append(in.Signers, list...)
	}
	if v := c.form("days"); v != "" {
		if in.ExpiresInDays, err = strconv.Atoi(v); err != nil {
			return invalid("days", "a whole number of days")
		}
		if in.ExpiresInDays == 0 {
			return invalid("days", "between 1 and %d", maxLinkDays)
		}
	}
	if _, err := u.Store.SendForSigning(c.ctx(), c.p, id, im, in); err != nil {
		return err
	}
	return c.redirect("/contracts/"+itoa(id), "flash.invited")
}

func (u *UI) signingRevoke(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	if _, err := u.Store.RevokeSigning(c.ctx(), c.p, id); err != nil {
		return err
	}
	return c.redirect("/contracts/"+itoa(id), "flash.withdrawn")
}

func (u *UI) signingResend(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	signer, err := c.id("signer")
	if err != nil {
		return err
	}
	e, err := u.Store.Resend(c.ctx(), c.p, id, signer)
	if err != nil {
		return err
	}
	flash := "flash.link_resent"
	if e.Status == EnvCompleted {
		flash = "flash.cert_resent"
	}
	return c.redirect("/contracts/"+itoa(id), flash)
}

func (u *UI) signingCertificate(c *uiCtx) error {
	id, err := c.id("id")
	if err != nil {
		return err
	}
	text, err := u.Store.Certificate(c.ctx(), c.p, id)
	if err != nil {
		return err
	}
	c.w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(c.w, text)
	return nil
}
