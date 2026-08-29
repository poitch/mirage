package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The browser view is read by whoever has an account, and on a family server
// that is not necessarily somebody who reads English. The admin page is left
// alone: one person runs the server and they chose to.
//
// Two languages and sixty phrases do not need a translation framework. A map
// and a template method carry it, and adding a language is adding a column.

// lang is a supported language.
type lang string

const (
	langEN lang = "en"
	langFR lang = "fr"
)

// defaultLang is used when the browser asks for nothing recognised.
const defaultLang = langEN

// supported lists what can be served, in the order they are preferred when a
// browser expresses no preference of its own.
var supported = []lang{langEN, langFR}

// languageFor picks a language from the Accept-Language header.
//
// The header is a list of tags with weights. Only the primary subtag is
// compared, so fr-CA is served the same French as fr-FR: the alternative is no
// French at all, which is worse than French from the wrong country.
func languageFor(r *http.Request) lang {
	type candidate struct {
		tag    string
		weight float64
	}
	var best candidate
	best.weight = -1

	for _, part := range strings.Split(r.Header.Get("Accept-Language"), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		tag, rest, _ := strings.Cut(part, ";")
		weight := 1.0
		if q, ok := strings.CutPrefix(strings.TrimSpace(rest), "q="); ok {
			if v, err := strconv.ParseFloat(strings.TrimSpace(q), 64); err == nil {
				weight = v
			}
		}
		tag = strings.ToLower(strings.TrimSpace(tag))
		primary, _, _ := strings.Cut(tag, "-")

		for _, s := range supported {
			if primary == string(s) && weight > best.weight {
				best = candidate{tag: string(s), weight: weight}
			}
		}
	}
	if best.weight <= 0 {
		return defaultLang
	}
	return lang(best.tag)
}

// t looks up a phrase for a request, for the messages built in Go rather than
// in a template.
func (s *Site) t(r *http.Request, key string) string {
	return pageData{Lang: languageFor(r)}.T(key)
}

// tf is t with values substituted in.
func (s *Site) tf(r *http.Request, key string, args ...any) string {
	return pageData{Lang: languageFor(r)}.Tf(key, args...)
}

// T looks up a phrase in the page's language.
//
// A missing translation falls back to English rather than to the key, so a
// phrase added and not yet translated reads awkwardly rather than looking
// broken.
func (p pageData) T(key string) string {
	if m, ok := messages[key]; ok {
		if s, ok := m[p.Lang]; ok && s != "" {
			return s
		}
		if s, ok := m[langEN]; ok {
			return s
		}
	}
	return key
}

// Tf is T with values substituted in.
func (p pageData) Tf(key string, args ...any) string {
	return fmt.Sprintf(p.T(key), args...)
}

// Date renders a timestamp in the page's language.
//
// Go formats month names in English and offers no way to change that, so a
// French page would otherwise date everything "Aug" - a small thing that makes
// the rest of the translation look like a veneer.
func (l lang) Date(t time.Time) string {
	t = t.Local()
	return fmt.Sprintf("%d %s %d, %02d:%02d",
		t.Day(), l.month(t.Month()), t.Year(), t.Hour(), t.Minute())
}

// DateOnly is Date without the time, for things dated to the day.
func (l lang) DateOnly(t time.Time) string {
	t = t.Local()
	return fmt.Sprintf("%d %s %d", t.Day(), l.month(t.Month()), t.Year())
}

func (l lang) month(m time.Month) string {
	if l == langFR {
		return frenchMonths[m-1]
	}
	return englishMonths[m-1]
}

var englishMonths = [...]string{
	"Jan", "Feb", "Mar", "Apr", "May", "Jun",
	"Jul", "Aug", "Sep", "Oct", "Nov", "Dec",
}

var frenchMonths = [...]string{
	"janv.", "févr.", "mars", "avril", "mai", "juin",
	"juil.", "août", "sept.", "oct.", "nov.", "déc.",
}

// messages is the catalogue. Keys are grouped by the page they appear on.
var messages = map[string]map[lang]string{
	// Shared furniture.
	"nav.files":       {langEN: "Files", langFR: "Fichiers"},
	"nav.trash":       {langEN: "Trash", langFR: "Corbeille"},
	"nav.signout":     {langEN: "Sign out", langFR: "Se déconnecter"},
	"col.name":        {langEN: "Name", langFR: "Nom"},
	"col.size":        {langEN: "Size", langFR: "Taille"},
	"col.modified":    {langEN: "Modified", langFR: "Modifié"},
	"action.download": {langEN: "Download", langFR: "Télécharger"},
	"action.restore":  {langEN: "Restore", langFR: "Restaurer"},

	// The landing page.
	"landing.title": {langEN: "Set up", langFR: "Configuration"},
	"landing.lead": {
		langEN: "Your files, on this server, kept in step with every device you use. Three steps and you are done.",
		langFR: "Vos fichiers, sur ce serveur, synchronisés avec tous vos appareils. Trois étapes et c’est fait.",
	},
	"landing.step1": {langEN: "Install the Nextcloud app", langFR: "Installez l’application Nextcloud"},
	"landing.step1.desc": {
		langEN: "Mirage speaks the Nextcloud protocol, so its app is the one to use.",
		langFR: "Mirage utilise le protocole Nextcloud : c’est donc cette application qu’il vous faut.",
	},
	"landing.platform": {
		langEN: "You appear to be on %s.",
		langFR: "Vous semblez être sur %s.",
	},
	"landing.others": {langEN: "Other devices:", langFR: "Autres appareils :"},
	"landing.step2":  {langEN: "Give it this address", langFR: "Indiquez-lui cette adresse"},
	"landing.step2.desc": {
		langEN: "The app asks for a server address when it first opens. This is it.",
		langFR: "L’application demande une adresse de serveur au premier lancement. La voici.",
	},
	"landing.step3": {langEN: "Sign in", langFR: "Connectez-vous"},
	"landing.step3.desc": {
		langEN: "Your username and password are the ones you were given for this server. A browser window opens to confirm it, and after that the app has its own credential — so changing your password later does not disconnect anything.",
		langFR: "Utilisez l’identifiant et le mot de passe qui vous ont été communiqués pour ce serveur. Une fenêtre s’ouvre pour confirmer, puis l’application dispose de son propre accès — changer votre mot de passe plus tard ne déconnectera donc rien.",
	},
	"landing.already": {langEN: "Already set up?", langFR: "Déjà configuré ?"},
	"landing.open":    {langEN: "Open your files in this browser", langFR: "Ouvrir vos fichiers dans ce navigateur"},

	// Platform names, so the sentence above reads naturally.
	"platform.iphone":  {langEN: "an iPhone", langFR: "un iPhone"},
	"platform.ipad":    {langEN: "an iPad", langFR: "un iPad"},
	"platform.android": {langEN: "Android", langFR: "Android"},
	"platform.mac":     {langEN: "a Mac", langFR: "un Mac"},
	"platform.windows": {langEN: "Windows", langFR: "Windows"},
	"platform.linux":   {langEN: "Linux", langFR: "Linux"},

	"client.ios":          {langEN: "Get it from the App Store", langFR: "Télécharger sur l’App Store"},
	"client.android":      {langEN: "Get it from Google Play", langFR: "Télécharger sur Google Play"},
	"client.desktop":      {langEN: "Download the desktop app", langFR: "Télécharger l’application bureau"},
	"client.name.ios":     {langEN: "iPhone or iPad", langFR: "iPhone ou iPad"},
	"client.name.android": {langEN: "Android", langFR: "Android"},
	"client.name.desktop": {langEN: "Mac, Windows or Linux", langFR: "Mac, Windows ou Linux"},

	// Signing in.
	"login.title": {langEN: "Sign in", langFR: "Connexion"},
	"login.sub": {
		langEN: "Use your Mirage account, the same one your sync client uses.",
		langFR: "Utilisez votre compte Mirage, celui de votre application de synchronisation.",
	},
	"login.username":   {langEN: "Username", langFR: "Identifiant"},
	"login.password":   {langEN: "Password", langFR: "Mot de passe"},
	"login.submit":     {langEN: "Sign in", langFR: "Se connecter"},
	"login.wrong":      {langEN: "Wrong username or password.", langFR: "Identifiant ou mot de passe incorrect."},
	"login.unreadable": {langEN: "That form could not be read.", langFR: "Ce formulaire n’a pas pu être lu."},

	// Browsing and searching.
	"browse.search":       {langEN: "Search your files by name", langFR: "Rechercher vos fichiers par nom"},
	"browse.searchButton": {langEN: "Search", langFR: "Rechercher"},
	"browse.matching":     {langEN: "Files matching", langFR: "Fichiers correspondant à"},
	"browse.nothing":      {langEN: "Nothing matches", langFR: "Aucun résultat pour"},
	"browse.back":         {langEN: "back to files", langFR: "retour aux fichiers"},
	"browse.empty":        {langEN: "This folder is empty.", langFR: "Ce dossier est vide."},
	"browse.versions":     {langEN: "Versions", langFR: "Versions"},
	"browse.root":         {langEN: "Files", langFR: "Fichiers"},
	"browse.in":           {langEN: "in %s", langFR: "dans %s"},
	"browse.topLevel":     {langEN: "the top level", langFR: "le dossier racine"},
	"browse.note": {
		langEN: "Mirage is a sync server. This page browses, downloads and restores; to work with your files, use the Nextcloud client.",
		langFR: "Mirage est un serveur de synchronisation. Cette page permet de parcourir, télécharger et restaurer ; pour travailler sur vos fichiers, utilisez l’application Nextcloud.",
	},

	// The trash.
	"trash.title":   {langEN: "Deleted files", langFR: "Fichiers supprimés"},
	"trash.wasIn":   {langEN: "was in %s", langFR: "était dans %s"},
	"trash.deleted": {langEN: "Deleted", langFR: "Supprimé"},
	"trash.forever": {langEN: "Delete for good", langFR: "Supprimer définitivement"},
	"trash.empty":   {langEN: "Nothing has been deleted.", langFR: "Aucun fichier supprimé."},
	"trash.note": {
		langEN: "Deleted files still take up your space until they are removed for good.",
		langFR: "Les fichiers supprimés occupent encore votre espace jusqu’à leur suppression définitive.",
	},
	"trash.restoredTo": {langEN: "Restored to %s.", langFR: "Restauré dans %s."},
	"trash.removed":    {langEN: "Removed for good.", langFR: "Supprimé définitivement."},
	"trash.gone":       {langEN: "That file is no longer in the trash.", langFR: "Ce fichier n’est plus dans la corbeille."},
	"trash.failed":     {langEN: "That file could not be restored.", langFR: "Ce fichier n’a pas pu être restauré."},

	// Versions.
	"versions.current": {langEN: "Current", langFR: "Version actuelle"},
	"versions.column":  {langEN: "Version", langFR: "Version"},
	"versions.none": {
		langEN: "This file has no earlier versions. One is kept each time it is overwritten.",
		langFR: "Ce fichier n’a pas de version antérieure. Une copie est conservée à chaque remplacement.",
	},
	"versions.note": {
		langEN: "Restoring keeps what is there now as another version, so nothing is lost either way.",
		langFR: "La restauration conserve la version actuelle comme une version de plus : rien n’est perdu dans un cas comme dans l’autre.",
	},
	"versions.restored": {langEN: "Restored the version from %s.", langFR: "Version du %s restaurée."},
	"versions.failed":   {langEN: "That version could not be restored.", langFR: "Cette version n’a pas pu être restaurée."},

	// The profile page.
	"profile.password": {langEN: "Password", langFR: "Mot de passe"},
	"profile.passwordHint": {
		langEN: "Used to sign in here and to pair a new device. Devices already set up keep working: each one has its own credential.",
		langFR: "Sert à vous connecter ici et à associer un nouvel appareil. Les appareils déjà configurés continuent de fonctionner : chacun possède son propre accès.",
	},
	"profile.current": {langEN: "Current password", langFR: "Mot de passe actuel"},
	"profile.new":     {langEN: "New password", langFR: "Nouveau mot de passe"},
	"profile.confirm": {langEN: "New password again", langFR: "Confirmez le nouveau mot de passe"},
	"profile.change":  {langEN: "Change password", langFR: "Changer le mot de passe"},
	"profile.changed": {
		langEN: "Password changed. Devices you have already set up are unaffected.",
		langFR: "Mot de passe modifié. Les appareils déjà configurés ne sont pas affectés.",
	},
	"profile.notCurrent": {langEN: "That is not your current password.", langFR: "Ce n’est pas votre mot de passe actuel."},
	"profile.mismatch":   {langEN: "The two new passwords do not match.", langFR: "Les deux nouveaux mots de passe ne correspondent pas."},
	"profile.tooShort":   {langEN: "A password must be at least %d characters.", langFR: "Un mot de passe doit comporter au moins %d caractères."},
	"profile.same":       {langEN: "That is the password you already have.", langFR: "C’est déjà votre mot de passe actuel."},

	"profile.addDevice": {langEN: "Add a device", langFR: "Ajouter un appareil"},
	"profile.addHint": {
		langEN: "Produces a code to scan in the Nextcloud app, which signs a phone or tablet in without typing an address or a password.",
		langFR: "Génère un code à scanner dans l’application Nextcloud, qui connecte un téléphone ou une tablette sans saisir d’adresse ni de mot de passe.",
	},
	"profile.scanHint": {
		langEN: "Open the Nextcloud app on the device, choose to sign in, and scan this. It works once and is not shown again.",
		langFR: "Ouvrez l’application Nextcloud sur l’appareil, choisissez de vous connecter, puis scannez ceci. Le code ne fonctionne qu’une fois et ne sera plus affiché.",
	},
	"profile.linkHint": {
		langEN: "If the app cannot scan, this is the same thing as a link:",
		langFR: "Si l’application ne peut pas scanner, voici le même contenu sous forme de lien :",
	},
	"profile.done":              {langEN: "Done", langFR: "Terminé"},
	"profile.whichDevice":       {langEN: "Which device?", langFR: "Quel appareil ?"},
	"profile.devicePlaceholder": {langEN: "e.g. my phone", langFR: "ex. mon téléphone"},
	"profile.showCode":          {langEN: "Show sign-in code", langFR: "Afficher le code de connexion"},
	"profile.devices":           {langEN: "Devices", langFR: "Appareils"},
	"profile.added":             {langEN: "Added", langFR: "Ajouté"},
	"profile.lastUsed":          {langEN: "last used %s", langFR: "dernière utilisation %s"},
	"profile.revoke":            {langEN: "Revoke", langFR: "Révoquer"},
	"profile.revokeNote": {
		langEN: "Revoking signs that device out. It will ask to be set up again.",
		langFR: "La révocation déconnecte cet appareil. Il faudra le configurer à nouveau.",
	},
	"profile.revoked":  {langEN: "That device has been signed out.", langFR: "Cet appareil a été déconnecté."},
	"profile.noDevice": {langEN: "That device could not be found.", langFR: "Cet appareil est introuvable."},
	"profile.unnamed":  {langEN: "Unnamed device", langFR: "Appareil sans nom"},
}
