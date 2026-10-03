// Copyright 2026 Brad Knight and contributors. Licensed under Apache-2.0. See LICENSE.
//
// steam_demos.go - the `steam demos` surface: a demos-only listing (app
// type = demo) that pairs each demo with the full game it belongs to.
// Free-to-play stays with `steam browse --free`; demos are a distinct app
// type here, not a price attribute.

package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/printing-press-library/library/media-and-entertainment/game-goat/internal/source/steam"
)

// steamDemoRow pairs a demo's full store record with the display name of the
// full game it belongs to. The parent appid already rides on StoreItem.
type steamDemoRow struct {
	steam.StoreItem
	ParentName string `json:"parent_name,omitempty"`
}

// steamDemoView is the demos envelope, shaped like the `steam browse` one.
type steamDemoView struct {
	Meta    steamMeta      `json:"meta"`
	Results []steamDemoRow `json:"results"`
}

// steamDemosLong explains the demos-only scope, the every-tag contract, the
// title batch's truncation, and the request budget.
const steamDemosLong = `Demos only: this command fixes the app type to "demo" and never returns
full games. Free-to-play titles are not demos - use "steam browse --free" for
those. Each row carries the full game the demo belongs to.

Every --tag must be present (the store service ANDs across tags). --title runs a
single text-search batch of up to 1000 results; that endpoint ignores offsets,
so the batch reports meta.truncated instead of a next page, and --page is
rejected with --title.

Request budget per invocation:
  - a browse page costs 2 requests: one catalog Query plus one GetItems lookup
    for the full-game names of every parent on the page;
  - each --tag adds one GetTagList request to turn tag names into ids;
  - a --title batch costs 1 SearchSuggestions request plus one name lookup per
    200 full games.

--coming-soon on the browse path swaps released-only for coming-soon-only. With
--title it filters the returned batch client-side, because the text-search
endpoint has no release filter.

` + steamCatalogLong

func newSteamDemosCmd(flags *rootFlags) *cobra.Command {
	var title, country, lang string
	var tags []string
	var comingSoon bool
	var limit, page int

	cmd := &cobra.Command{
		Use:   "demos",
		Short: "Find free demos available on Steam (demos only), with the full game each belongs to",
		Long:  steamDemosLong,
		Example: strings.Trim(`
  game-goat-pp-cli steam demos --limit 20 --agent
  game-goat-pp-cli steam demos --tag Roguelike --tag Metroidvania --country DE
  game-goat-pp-cli steam demos --title portal --limit 1000 --json
`, "\n"),
		Annotations: map[string]string{
			"mcp:read-only":  "true",
			"pp:data-source": "live",
			"pp:happy-args":  "--limit=20;--dry-run",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 && cmd.Flags().NFlag() == 0 {
				return cmd.Help()
			}
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "steam demos")
			}
			title = strings.TrimSpace(title)
			if page < 1 {
				return usageErrWithJSON(cmd, flags, cmd.CommandPath()+" --page <n>", "--page must be 1 or greater")
			}
			if title != "" {
				if page != 1 {
					return usageErrWithJSON(cmd, flags, cmd.CommandPath()+" --title <term>",
						"--page cannot be used with --title: the Steam text-search endpoint ignores offsets, so a title batch is always one page (raise --limit instead)")
				}
				if limit < 1 || limit > steam.MaxSearchBatch {
					return usageErrWithJSON(cmd, flags, cmd.CommandPath()+" --title <term> --limit <1-1000>", "--limit must be between 1 and 1000 with --title")
				}
			} else if limit < 1 || limit > steam.MaxPageSize {
				return usageErrWithJSON(cmd, flags, cmd.CommandPath()+" --limit <1-100>", "--limit must be between 1 and 100")
			}
			resolvedCountry, cerr := resolveSteamCountry(country)
			if cerr != nil {
				return usageErrWithJSON(cmd, flags, cmd.CommandPath()+" --country <iso>", cerr.Error())
			}
			resolvedLang := resolveSteamLanguage(lang)
			ctx, cancel := boundCtx(cmd.Context(), flags)
			defer cancel()

			c := newSteamClient(resolvedCountry, resolvedLang)
			tagIDs := make([]int, 0, len(tags))
			for _, t := range tags {
				id, rerr := c.ResolveTag(ctx, t)
				if rerr != nil {
					return usageErrWithJSON(cmd, flags, cmd.CommandPath()+" --tag <name-or-id>", rerr.Error())
				}
				tagIDs = append(tagIDs, id)
			}

			meta := steamMeta{
				Source:   "live",
				Country:  resolvedCountry,
				Language: resolvedLang,
				Types:    []string{string(steam.AppTypeDemo)},
				Tags:     tags,
				Limit:    limit,
			}
			var items []steam.StoreItem

			if title != "" {
				p, serr := c.SearchPage(ctx, title, steam.SearchPageOptions{
					Types:  []steam.AppType{steam.AppTypeDemo},
					TagIDs: tagIDs,
					Limit:  limit,
				})
				if serr != nil {
					return classifySteamError(serr)
				}
				items = p.Items
				meta.Title = title
				meta.Total = p.Total
				truncated := p.Truncated()
				meta.Truncated = &truncated
				if comingSoon {
					items = filterComingSoonDemos(items)
					meta.Total = len(items)
				}
			} else {
				result, berr := c.Browse(ctx, steam.BrowseOptions{
					Types:        []steam.AppType{steam.AppTypeDemo},
					TagIDs:       tagIDs,
					ComingSoon:   comingSoon,
					ReleasedOnly: !comingSoon,
					Start:        (page - 1) * limit,
					Count:        limit,
					SkipTagNames: true,
				})
				if berr != nil {
					return classifySteamError(berr)
				}
				items = result.Items
				meta.Total = result.Total
				meta.Page = page
				if result.HasMore() {
					meta.NextPage = page + 1
				}
			}

			rows := demoRows(items)
			if _, perr := attachDemoParentNames(ctx, c, rows); perr != nil {
				meta.SourcesMissing = append(meta.SourcesMissing, "steam_parent")
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: steam full-game names unavailable (sources_missing: steam_parent): %v\n", perr)
			}

			if !wantsHumanTable(cmd.OutOrStdout(), flags) {
				return printJSONFiltered(cmd.OutOrStdout(), steamDemoView{Meta: meta, Results: rows}, flags)
			}
			return renderSteamDemoItems(cmd, steamDemoHeading(title, meta, len(rows), page), rows)
		},
	}

	cmd.Flags().StringVar(&title, "title", "", "Only demos whose title matches this term (one batch, up to 1000; the text-search endpoint ignores offsets, so --page is rejected with --title)")
	cmd.Flags().StringSliceVar(&tags, "tag", nil, "Store tag name or tagid that every result must carry (repeatable or comma-separated, e.g. --tag Roguelike,Metroidvania)")
	cmd.Flags().BoolVar(&comingSoon, "coming-soon", false, "Only unreleased demos (browse path swaps released-only for coming-soon-only)")
	cmd.Flags().IntVar(&limit, "limit", 20, "Results per page (1-100 without --title, 1-1000 with --title)")
	cmd.Flags().IntVar(&page, "page", 1, "Page number, 1-based (browse only; rejected with --title)")
	cmd.Flags().StringVar(&country, "country", "", "ISO 3166-1 alpha-2 storefront region (default STEAM_COUNTRY, ITAD_COUNTRY, or US)")
	cmd.Flags().StringVar(&lang, "lang", "", "Store locale for store text (default english)")
	return cmd
}

func demoRows(items []steam.StoreItem) []steamDemoRow {
	rows := make([]steamDemoRow, 0, len(items))
	for _, item := range items {
		rows = append(rows, steamDemoRow{StoreItem: item})
	}
	return rows
}

// attachDemoParentNames fills ParentName from one AppNames lookup over the
// unique parent appids on the page. A failure is reported to the caller (so it
// can degrade to sources_missing) but never drops the demo rows.
func attachDemoParentNames(ctx context.Context, c *steam.Client, rows []steamDemoRow) ([]int64, error) {
	seen := map[int64]bool{}
	var ids []int64
	for _, row := range rows {
		if row.ParentAppID > 0 && !seen[row.ParentAppID] {
			seen[row.ParentAppID] = true
			ids = append(ids, row.ParentAppID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	names, err := c.AppNames(ctx, ids)
	if err != nil {
		return ids, err
	}
	for i := range rows {
		if name, ok := names[rows[i].ParentAppID]; ok {
			rows[i].ParentName = name
		}
	}
	return ids, nil
}

// filterComingSoonDemos is the client-side fallback for --coming-soon on the
// title path, which has no server release filter.
func filterComingSoonDemos(items []steam.StoreItem) []steam.StoreItem {
	out := make([]steam.StoreItem, 0, len(items))
	for _, item := range items {
		if item.ComingSoon {
			out = append(out, item)
		}
	}
	return out
}

func steamDemoHeading(title string, meta steamMeta, count, page int) string {
	if title != "" {
		suffix := ""
		if meta.Truncated != nil && *meta.Truncated {
			suffix = ", truncated"
		}
		return fmt.Sprintf("Steam demos matching %q (%d of %d%s)", title, count, meta.Total, suffix)
	}
	return fmt.Sprintf("Steam demos (%d of %d, page %d, %s)", count, meta.Total, page, meta.Country)
}

// renderSteamDemoItems is the human table for demos: the same columns as the
// other steam commands plus the full game each demo belongs to.
func renderSteamDemoItems(cmd *cobra.Command, heading string, rows []steamDemoRow) error {
	w := cmd.OutOrStdout()
	fmt.Fprintln(w, heading)
	if len(rows) == 0 {
		fmt.Fprintln(w, "no Steam demos matched")
		return nil
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		parent := row.ParentName
		if parent == "" {
			parent = "-"
		}
		out = append(out, map[string]any{
			"appid":     row.AppID,
			"name":      row.Name,
			"full game": parent,
			"released":  orDash(row.ReleaseDate),
			"platforms": steamPlatformLabel(row.StoreItem),
			"flags":     steamFlagLabel(row.StoreItem),
		})
	}
	return printAutoTable(w, out)
}

// steamPlatformLabel lists the desktop platforms the store marks available.
func steamPlatformLabel(item steam.StoreItem) string {
	var oses []string
	if item.Platforms.Windows {
		oses = append(oses, "windows")
	}
	if item.Platforms.Mac {
		oses = append(oses, "mac")
	}
	if item.Platforms.Linux {
		oses = append(oses, "linux")
	}
	if len(oses) == 0 {
		return "-"
	}
	return strings.Join(oses, ",")
}
