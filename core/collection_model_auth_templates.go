package core

// Common settings placeholder tokens
const (
	EmailPlaceholderAppName   string = "{APP_NAME}"
	EmailPlaceholderAppURL    string = "{APP_URL}"
	EmailPlaceholderToken     string = "{TOKEN}"
	EmailPlaceholderOTP       string = "{OTP}"
	EmailPlaceholderOTPId     string = "{OTP_ID}"
	EmailPlaceholderAlertInfo string = "{ALERT_INFO}"
)

var defaultVerificationTemplate = EmailTemplate{
	Subject: "Verify your " + EmailPlaceholderAppName + " email",
	Body: `<p>Hello,</p>
<p>Thank you for joining us at ` + EmailPlaceholderAppName + `.</p>
<p>Click on the button below to verify your email address.</p>
<p>
  <a class="btn" href="` + EmailPlaceholderAppURL + "/_/#/auth/confirm-verification/" + EmailPlaceholderToken + `" target="_blank" rel="noopener">Verify</a>
</p>
<p><i>If you didn't recently register, please ignore this email.</i></p>
<p>
  Thanks,<br/>
  ` + EmailPlaceholderAppName + ` team
</p>`,
}

var defaultResetPasswordTemplate = EmailTemplate{
	Subject: "Reset your " + EmailPlaceholderAppName + " password",
	Body: `<p>Hello,</p>
<p>Click on the button below to reset your password.</p>
<p>
  <a class="btn" href="` + EmailPlaceholderAppURL + "/_/#/auth/confirm-password-reset/" + EmailPlaceholderToken + `" target="_blank" rel="noopener">Reset password</a>
</p>
<p><i>If you didn't ask to reset your password, please ignore this email.</i></p>
<p>
  Thanks,<br/>
  ` + EmailPlaceholderAppName + ` team
</p>`,
}

var defaultConfirmEmailChangeTemplate = EmailTemplate{
	Subject: "Confirm your " + EmailPlaceholderAppName + " new email address",
	Body: `<p>Hello,</p>
<p>Click on the button below to confirm your new email address.</p>
<p>
  <a class="btn" href="` + EmailPlaceholderAppURL + "/_/#/auth/confirm-email-change/" + EmailPlaceholderToken + `" target="_blank" rel="noopener">Confirm new email</a>
</p>
<p><i>If you didn't ask to change your email address, please ignore this email.</i></p>
<p>
  Thanks,<br/>
  ` + EmailPlaceholderAppName + ` team
</p>`,
}

var defaultOTPTemplate = EmailTemplate{
	Subject: "OTP for " + EmailPlaceholderAppName,
	Body: `<p>Hello,</p>
<p>Your one-time password is: <strong>` + EmailPlaceholderOTP + `</strong></p>
<p><i>If you didn't ask for the one-time password, you can ignore this email.</i></p>
<p>
  Thanks,<br/>
  ` + EmailPlaceholderAppName + ` team
</p>`,
}

var defaultAuthAlertTemplate = EmailTemplate{
	Subject: "Login from a new location",
	Body: `<p>Hello,</p>
<p>We noticed a login to your ` + EmailPlaceholderAppName + ` account from a new location:</p>
<p><em>` + EmailPlaceholderAlertInfo + `</em></p>
<p><strong>If this wasn't you, you should immediately change your ` + EmailPlaceholderAppName + ` account password to revoke access from all other locations.</strong></p>
<p>If this was you, you may disregard this email.</p>
<p>
  Thanks,<br/>
  ` + EmailPlaceholderAppName + ` team
</p>`,
}

var defaultPasskeyResetTemplate = EmailTemplate{
	Subject: "Reset your " + EmailPlaceholderAppName + " passkey",
	Body: `<p>Hello,</p>
<p>Click on the button below to register a new passkey on this device. This link will replace any previously registered passkeys for your account.</p>
<p>
  <a class="btn" href="` + EmailPlaceholderAppURL + "/_/#/auth/confirm-passkey-reset/" + EmailPlaceholderToken + `" target="_blank" rel="noopener">Reset passkey</a>
</p>
<p><i>If you didn't ask to reset your passkey, you can ignore this email.</i></p>
<p>
  Thanks,<br/>
  ` + EmailPlaceholderAppName + ` team
</p>`,
}
