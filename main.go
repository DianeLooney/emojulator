package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"image/png"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/ftrvxmtrx/tga"
	"github.com/getsentry/sentry-go"
	"github.com/nfnt/resize"
	"github.com/pkg/errors"
)

var token string
var sentryURL string
var redirectURL string
var discord *discordgo.Session
var port string

func init() {
	token = os.Getenv("DISCORD_TOKEN")
	sentryURL = os.Getenv("SENTRY_URL")
	port = os.Getenv("PORT")
	redirectURL = os.Getenv("REDIRECT_URL")
}

func main() {
	if err := sentry.Init(sentry.ClientOptions{Dsn: sentryURL}); err != nil {
		log.Fatalf("sentry.Init: %s", err)
	}
	defer sentry.Flush(2 * time.Second)

	
	var err error
	discord, err = discordgo.New("Bot " + token)
	if err != nil {
		fmt.Println("error creating Discord session,", err)
		return
	}
	discord.AddHandler(messageCreate)

	if err := discord.Open(); err != nil {
		fmt.Println("error opening connection,", err)
		return
	}
	defer discord.Close()
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectURL, http.StatusTemporaryRedirect)
	})
	http.ListenAndServe(":"+port, nil)
}

func messageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	// set up error reporting
	var err error
	sentryHub := sentry.CurrentHub().Clone()
	defer func() {
		dump := func(id *sentry.EventID) {
			if id == nil {
				s.ChannelMessageSend(m.ChannelID, "Unable to generate emoji. ☹️")
			} else {
				s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("Unable to generate emoji. ☹️\nerror_id: %v", *id))
			}
		}

		if err != nil {
			id := sentryHub.CaptureException(err)
			dump(id)
			return
		}
		if r := recover(); r != nil {
			id := sentryHub.Recover(r)
			dump(id)
			return
		}
	}()

	// only respond to messages from non-bots that are `!emojulate`
	if m.Author.Bot {
		return
	}
	
	if m.Content != "!emojulate" {
		return
	}

	// add context to error reporting
	sentryHub.ConfigureScope(func(scope *sentry.Scope) {
		scope.SetUser(sentry.User{
			ID: m.Author.ID,
		})
		scope.SetTag("channel.id", m.ChannelID)
		scope.SetTag("guild.id", m.GuildID)
	})

	if _, err = s.ChannelMessageSend(m.ChannelID, "Generating..."); err != nil {
		err = errors.Wrap(err, "unable to send message to channel")
		return
	}

	guild, err := discord.Guild(m.GuildID)
	if err != nil {
		err = errors.Wrap(err, "unable to retrieve guild info")
		return
	}

	buf := new(bytes.Buffer)
	zipWriter := zip.NewWriter(buf)
	packName := fmt.Sprintf("TwitchEmotes - %v", guild.Name)

	// copy the template data to the zip
	err = filepath.Walk("./DiscordEmotes", func(path string, info os.FileInfo, err error) error {
		if path == "./DiscordEmotes" {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		
		data, err := ioutil.ReadFile(path)
		if err != nil {
			return errors.Wrap(err, "unable to read source file")
		}
		if strings.Contains(path, "DiscordEmotes.lua") {
			// TODO: Use a template for sappho sake.
			data = bytes.Replace(data, []byte("discord_server_id"), []byte(packName), 1)
			for _, e := range guild.Emojis {
				nsEmoji := fmt.Sprintf("discord.%v.%v", guild.ID, e.Name)
				data = bytes.Replace(data, []byte("--Pack"), []byte(fmt.Sprintf("['%v']='Interface\\\\AddOns\\\\%v\\\\%v\\\\%v.tga:28:28',\n--Pack", nsEmoji, packName, g.ID, e.Name)), 1)
				data = bytes.Replace(data, []byte("--Emoticons"), []byte(fmt.Sprintf("['%v']='%v',\n--Emoticons", ":"+e.Name+":", nsEmoji)), 1)
			}
		}
		if strings.Contains(path, "DiscordEmotes.toc") {
			data = bytes.Replace(data, []byte("## Title: DiscordEmotes"), []byte(fmt.Sprintf("## Title: %v", packName)), 1)
		}

		remappedPath := strings.ReplaceAll(path, "DiscordEmotes", packName)
		writer, err := zipWriter.Create(remappedPath)
		if err != nil {
			return errors.Wrap(err, "unable to create")
		}

		_, err = writer.Write(data)
		if err != nil {
			err = errors.Wrap(err, "unable to write data to zip")
		}

		return err
	})

	// add emojis to the zip
	for _, e := range guild.Emojis {
		loc := fmt.Sprintf("https://cdn.discordapp.com/emojis/%v.png", e.ID)
		client := http.Client{}
		d, err := client.Get(loc)
		if err != nil {
			err = errors.Wrap(err, "unable to download emoji")
			return
		}
		image, err := png.Decode(d.Body)
		if err != nil {
			err = errors.Wrap(err, "unable to decode as png")
			return
		}
		image = resize.Resize(32, 32, image, resize.Lanczos3)
		w, err := zipWriter.Create(fmt.Sprintf("%v\\%v\\%v.tga", packName, guild.ID, e.Name))
		if err != nil {
			err = errors.Wrap(err, "unable to add file to zip")
			return
		}
		err = tga.Encode(w, image)
		if err != nil {
			err = errors.Wrap(err, "unable to encode image as tga")
			return
		}
	}
	err = zipWriter.Close()
	if err != nil {
		err = errors.Wrap(err, "unable to close zipwriter")
		return
	}

	// upload emoji pack to the server
	_, err = s.ChannelFileSendWithMessage(m.ChannelID, "All done! 🎉", fmt.Sprintf("%v.zip", packName), buf)
	if err != nil {
		err = errors.Wrap(err, "unable to send addon to channel")
	}
}
