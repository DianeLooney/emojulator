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
	// start error tracking
	if err := sentry.Init(sentry.ClientOptions{Dsn: sentryURL}); err != nil {
		log.Fatalf("sentry.Init: %s", err)
	}
	defer sentry.Flush(2 * time.Second)

	// connect to the discord api
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

	// not easy to see here from this, but this makes it so:
	// https://emojulator.diane.af and https://emojulator-beta.diane.af
	// redirects users to add the bot to their server 
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectURL, http.StatusTemporaryRedirect)
	})
	http.ListenAndServe(":"+port, nil)
}

func messageCreate(session *discordgo.Session, message *discordgo.MessageCreate) {
	// set up error reporting
	var err error
	sentryHub := sentry.CurrentHub().Clone()
	defer func() {
		dump := func(id *sentry.EventID) {
			if id == nil {
				session.ChannelMessageSend(message.ChannelID, "Unable to generate emoji. ☹️")
			} else {
				session.ChannelMessageSend(message.ChannelID, fmt.Sprintf("Unable to generate emoji. ☹️\nerror_id: %v", *id))
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
	if message.Author.Bot {
		return
	}
	
	if message.Content != "!emojulate" {
		return
	}

	// add context to error reporting
	sentryHub.ConfigureScope(func(scope *sentry.Scope) {
		scope.SetUser(sentry.User{
			ID: message.Author.ID,
		})
		scope.SetTag("channel.id", message.ChannelID)
		scope.SetTag("guild.id", message.GuildID)
	})

	if _, err = session.ChannelMessageSend(message.ChannelID, "Generating..."); err != nil {
		err = errors.Wrap(err, "unable to send message to channel")
		return
	}

	guild, err := discord.Guild(message.GuildID)
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

		// If this is core.lua we have to insert the Pack and Emoticons into the file
		if strings.Contains(path, "core.lua") {
			// TODO: Use a template for sappho sake.
			data = bytes.Replace(data, []byte("discord_server_id"), []byte(packName), 1)
			for _, e := range guild.Emojis {
				nsEmoji := fmt.Sprintf("discord.%v.%v", guild.ID, e.Name)
				data = bytes.Replace(data, []byte("--Pack"), []byte(fmt.Sprintf("['%v']='Interface\\\\AddOns\\\\%v\\\\%v\\\\%v.tga:28:28',\n--Pack", nsEmoji, packName, guild.ID, e.Name)), 1)
				data = bytes.Replace(data, []byte("--Emoticons"), []byte(fmt.Sprintf("['%v']='%v',\n--Emoticons", ":"+e.Name+":", nsEmoji)), 1)
			}
		}

		// If this is the toc we have to update the title so you can read it in the wow interface
		if strings.Contains(path, "DiscordEmotes.toc") {
			data = bytes.Replace(data, []byte("## Title: DiscordEmotes"), []byte(fmt.Sprintf("## Title: %v", packName)), 1)
		}

		// Without changing paths all addons downloaded from this bot would look the same
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
		downloadPath := fmt.Sprintf("https://cdn.discordapp.com/emojis/%v.png", e.ID)

		// download the emoji
		client := http.Client{}
		d, err := client.Get(downloadPath)
		if err != nil {
			err = errors.Wrap(err, "unable to download emoji")
			return
		}

		// convert it from a png to an image we can manipulate
		image, err := png.Decode(d.Body)
		if err != nil {
			err = errors.Wrap(err, "unable to decode as png")
			return
		}

		// resize the image to 32x32 so wow will read it
		image = resize.Resize(32, 32, image, resize.Lanczos3)

		// create the file so we can write it to the zip
		w, err := zipWriter.Create(fmt.Sprintf("%v\\%v\\%v.tga", packName, guild.ID, e.Name))
		if err != nil {
			err = errors.Wrap(err, "unable to add file to zip")
			return
		}

		// encode the image as tga directly into the zip
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
	_, err = session.ChannelFileSendWithMessage(message.ChannelID, "All done! 🎉", fmt.Sprintf("%v.zip", packName), buf)
	if err != nil {
		err = errors.Wrap(err, "unable to send addon to channel")
	}
}
