# Contributing

This file covers monbooru and its companion repos. Start from what you want to do and follow the arrows.

AI-generated contributions that their author has obviously not reviewed or put effort into are closed without review.

```mermaid
---
config:
  layout: dagre
  flowchart:
    wrappingWidth: 220
---
flowchart LR
    start(["`**You want to...**`"])
    start --> a["`**report a bug** or **suggest a feature**
    → Report it`"]
    start --> f["`**ask a question** or **share your setup**
    → Ask a question`"]
    start --> b["`**change the code**
    → Change the code`"]
    start --> c["`**add a tagger model**, or fix a shipped one
    → Taggers`"]
    start --> d["`**add a site** to monloader, or fix one
    → Sites`"]
    start --> e["`**share a plugin or theme**
    → Plugins and themes`"]

    class start hub;
    class a,b,c,d,e,f case;

    classDef hub  fill:#9d2235,stroke:#ef8f99,stroke-width:3px,color:#ffffff;
    classDef case fill:#3a161c,stroke:#ef8f99,stroke-width:1.5px,color:#ffffff;
    classDef step fill:#1a1818,stroke:#9d2235,stroke-width:1.5px,color:#e8e4e2;
    classDef ask  fill:#1a1818,stroke:#a09898,stroke-width:1px,stroke-dasharray:4 3,color:#e8e4e2;
    classDef repo fill:#1a1818,stroke:#a09898,stroke-width:1px,color:#a09898;
```

## Ask a question

Questions, workflows, setups and general talk go to [Discussions](https://github.com/monbooru/monbooru/discussions), for every repo. There is no Discord or other chat server, so answers stay public and the next person can find them.

## Report it

```mermaid
---
config:
  layout: dagre
  flowchart:
    wrappingWidth: 220
---
flowchart TD
    want{{"You want to..."}}
    want -->|"report a bug"| bug["`**Give the details**
    version, binary or Docker, steps to reproduce, what you expected, log lines or screenshots`"]
    want -->|"suggest a feature"| idea["`**Start from the problem**
    describe the problem or workflow first, then the solution you have in mind`"]
    bug & idea --> issue["`**Open an issue in the repo it is about**
    see the graph below`"]
    issue -->|"it leads to a change"| ship(["`**Ships** → Credit`"])

    class ship hub;
    class bug,idea,issue step;
    class want ask;

    classDef hub  fill:#9d2235,stroke:#ef8f99,stroke-width:3px,color:#ffffff;
    classDef case fill:#3a161c,stroke:#ef8f99,stroke-width:1.5px,color:#ffffff;
    classDef step fill:#1a1818,stroke:#9d2235,stroke-width:1.5px,color:#e8e4e2;
    classDef ask  fill:#1a1818,stroke:#a09898,stroke-width:1px,stroke-dasharray:4 3,color:#e8e4e2;
    classDef repo fill:#1a1818,stroke:#a09898,stroke-width:1px,color:#a09898;
```

```mermaid
---
config:
  layout: dagre
  flowchart:
    wrappingWidth: 220
---
flowchart LR
    about{{"It is about..."}}
    about -->|"the app, the plugin contract"| booru["`**monbooru**`"]
    about -->|"downloading, sites, reverse lookup"| loader["`**monloader**`"]
    about -->|"the browser extension"| sender["`**monsender**`"]
    about -->|"the documentation"| docs["`**mondocs**`"]
    about -->|"the listings, a shipped tagger"| plugins["`**monbooru-plugins**`"]
    about -->|"a plugin or theme someone else listed"| third["`**that plugin's own tracker**`"]

    class about ask;
    class booru,loader,sender,docs,plugins,third repo;

    classDef hub  fill:#9d2235,stroke:#ef8f99,stroke-width:3px,color:#ffffff;
    classDef case fill:#3a161c,stroke:#ef8f99,stroke-width:1.5px,color:#ffffff;
    classDef step fill:#1a1818,stroke:#9d2235,stroke-width:1.5px,color:#e8e4e2;
    classDef ask  fill:#1a1818,stroke:#a09898,stroke-width:1px,stroke-dasharray:4 3,color:#e8e4e2;
    classDef repo fill:#1a1818,stroke:#a09898,stroke-width:1px,color:#a09898;
```

Repos: [monbooru](https://github.com/monbooru/monbooru) · [monloader](https://github.com/monbooru/monloader) · [monsender](https://github.com/monbooru/monsender) · [mondocs](https://github.com/monbooru/mondocs) · [monbooru-plugins](https://github.com/monbooru/monbooru-plugins)

References:

- [Troubleshooting (monbooru)](https://monbooru.github.io/mondocs/advanced/troubleshooting.html)
- [Troubleshooting (monsender)](https://monbooru.github.io/mondocs/addons/monsender/troubleshooting.html)

## Change the code

```mermaid
---
config:
  layout: dagre
  flowchart:
    wrappingWidth: 220
---
flowchart TD
    size{{"How big is it?"}}
    size -->|"a small fix"| branch
    size -->|"anything bigger, a new dependency, or changes across many templates or handlers"| ask["`**Open an issue first**
    say what and why, wait for a go-ahead`"]
    ask --> branch["`**Write it on a branch of your fork**
    follow the existing style, reuse the patterns the code already has, and follow any instructions given in the issue`"]
    branch --> self["`**Read every line of your diff and test it live**`"]
    self --> link["`**Link the branch from the issue**
    a small fix opens its issue now`"]
    link --> review{{"Review answers in the issue or PR"}}
    review -->|"needs changes"| branch
    review -->|"declined"| declined["`Closed, with the reasons`"]
    review -->|"accepted, whole or in part"| ship(["`**Applied upstream, ships in the next release**
    the PR is closed then, never merged in the GitHub UI → Credit`"])

    class ship hub;
    class ask,branch,self,link,declined step;
    class size,review ask;

    classDef hub  fill:#9d2235,stroke:#ef8f99,stroke-width:3px,color:#ffffff;
    classDef case fill:#3a161c,stroke:#ef8f99,stroke-width:1.5px,color:#ffffff;
    classDef step fill:#1a1818,stroke:#9d2235,stroke-width:1.5px,color:#e8e4e2;
    classDef ask  fill:#1a1818,stroke:#a09898,stroke-width:1px,stroke-dasharray:4 3,color:#e8e4e2;
    classDef repo fill:#1a1818,stroke:#a09898,stroke-width:1px,color:#a09898;
```

This repo publishes squashed release snapshots, which is why accepted work lands through a release rather than a merge.

References:

- [Building monbooru from source](https://monbooru.github.io/mondocs/advanced/building.html)
- [Building monloader from source](https://monbooru.github.io/mondocs/addons/monloader/development.html#building-from-source)
- [REST API and tokens](https://monbooru.github.io/mondocs/advanced/api.html)
- [monloader API and error codes](https://monbooru.github.io/mondocs/addons/monloader/development.html#api-tokens)

## Taggers

```mermaid
---
config:
  layout: dagre
  flowchart:
    wrappingWidth: 220
---
flowchart TD
    want{{"You want to..."}}
    want -->|"ask for a new model"| req["`**Issue in monbooru-plugins**
    a link to the model`"]
    want -->|"report a shipped one that tags badly"| bug["`→ Report it`"]
    want -->|"add or fix it yourself"| fmt
    fmt{{"Does monbooru already read its label-file format and category scheme?"}}
    fmt -->|"no"| code["`**Needs code**
    open a monbooru issue first → Change the code`"]
    fmt -->|"yes"| tune["`**Get it working locally**
    put it under model_path/*name*/ and tune its tagger.json and dispatch.json sidecars; dispatch rules can be edited in the tagger's mapping tab`"]
    tune -->|"PR"| pr["`**PR to monbooru-plugins**
    taggers/*name*/ with your sidecars as profile_default.json and dispatch_default.json, and a model.json with public download URLs`"]
    tune -->|"rather not touch the repo"| issue["`**Issue in monbooru-plugins**
    the model link and the settings that worked`"]
    req & pr & issue --> ship(["`**Ships** → Credit`"])

    class ship hub;
    class code,tune,pr,issue,req step;
    class bug case;
    class want,fmt ask;

    classDef hub  fill:#9d2235,stroke:#ef8f99,stroke-width:3px,color:#ffffff;
    classDef case fill:#3a161c,stroke:#ef8f99,stroke-width:1.5px,color:#ffffff;
    classDef step fill:#1a1818,stroke:#9d2235,stroke-width:1.5px,color:#e8e4e2;
    classDef ask  fill:#1a1818,stroke:#a09898,stroke-width:1px,stroke-dasharray:4 3,color:#e8e4e2;
    classDef repo fill:#1a1818,stroke:#a09898,stroke-width:1px,color:#a09898;
```

References:

- [TAGGERS.md: the model.json, profile and dispatch formats](https://github.com/monbooru/monbooru-plugins/blob/main/TAGGERS.md)
- [The shipped tagger folders, as examples](https://github.com/monbooru/monbooru-plugins/tree/main/taggers)
- [Your own models](https://monbooru.github.io/mondocs/advanced/auto-tagger.html#your-own-models)
- [Label rules (dispatch.json)](https://monbooru.github.io/mondocs/advanced/auto-tagger.html#label-rules-dispatchjson)
- [Auto-tagger guide: installing and tuning a model](https://monbooru.github.io/mondocs/guides/auto-tagger.html)

## Sites

```mermaid
---
config:
  layout: dagre
  flowchart:
    wrappingWidth: 220
---
flowchart TD
    want{{"You want to..."}}
    want -->|"ask for a new site"| req["`**Issue in monloader**
    the site and an example post URL that works without an account; it has to be a site gallery-dl supports`"]
    want -->|"report a site that maps badly"| bug["`→ Report it`"]
    want -->|"add or fix it yourself"| build
    build["`**Build the profile in monloader**
    Settings → Sites, add the site, tune it in the site dialog; monloader saves profiles/*category*.json next to monloader.toml`"]
    build -->|"PR"| pr["`**PR to monloader**
    your file as-is under internal/mapping/profiles/`"]
    build -->|"rather not touch the repo"| issue["`**Issue in monloader**
    the JSON pasted in`"]
    pr & issue --> url["`Add an example post URL that works without an account, when there is one`"]
    url & req --> ship(["`**Ships** → Credit`"])

    class ship hub;
    class build,pr,issue,url,req step;
    class bug case;
    class want ask;

    classDef hub  fill:#9d2235,stroke:#ef8f99,stroke-width:3px,color:#ffffff;
    classDef case fill:#3a161c,stroke:#ef8f99,stroke-width:1.5px,color:#ffffff;
    classDef step fill:#1a1818,stroke:#9d2235,stroke-width:1.5px,color:#e8e4e2;
    classDef ask  fill:#1a1818,stroke:#a09898,stroke-width:1px,stroke-dasharray:4 3,color:#e8e4e2;
    classDef repo fill:#1a1818,stroke:#a09898,stroke-width:1px,color:#a09898;
```

References:

- [Site profiles: the file format](https://monbooru.github.io/mondocs/addons/monloader/development.html#site-profiles)
- [Editing a site's profile in the app](https://monbooru.github.io/mondocs/addons/monloader/guides/mapping.html#editing-a-sites-profile)
- [Setting up a site and its credentials](https://monbooru.github.io/mondocs/addons/monloader/guides/sites.html#setting-up-a-site)
- [The shipped profiles, as examples](https://github.com/monbooru/monloader/tree/main/internal/mapping/profiles)

## Plugins and themes

```mermaid
---
config:
  layout: dagre
  flowchart:
    wrappingWidth: 220
---
flowchart TD
    want{{"You want to..."}}
    want -->|"share a plugin or theme"| own["`**Publish it in your own repo**
    it needs no change in monbooru: build it against the plugin contract, in any language`"]
    own --> list["`**PR one row to PLUGINS.md or THEMES.md** in monbooru-plugins
    listing is not a review`"]
    want -->|"report a bug in a listed plugin or theme"| third["`**Its own tracker**`"]
    want -->|"report a contract problem, or ask for new surface"| booru["`**Issue in monbooru**`"]

    class own,list,third,booru step;
    class want ask;

    classDef hub  fill:#9d2235,stroke:#ef8f99,stroke-width:3px,color:#ffffff;
    classDef case fill:#3a161c,stroke:#ef8f99,stroke-width:1.5px,color:#ffffff;
    classDef step fill:#1a1818,stroke:#9d2235,stroke-width:1.5px,color:#e8e4e2;
    classDef ask  fill:#1a1818,stroke:#a09898,stroke-width:1px,stroke-dasharray:4 3,color:#e8e4e2;
    classDef repo fill:#1a1818,stroke:#a09898,stroke-width:1px,color:#a09898;
```

References:

- [Writing a plugin](https://monbooru.github.io/mondocs/advanced/api.html#writing-a-plugin)
- [Pairing](https://monbooru.github.io/mondocs/addons/pairing.html)
- [Plugins: button kinds and installing](https://monbooru.github.io/mondocs/addons/plugins.html)
- [Themes: making your own and the variables](https://monbooru.github.io/mondocs/advanced/themes.html)
- [simple-edit, an example plugin](https://github.com/monbooru/monbooru-plugins/tree/main/examples/simple-edit)
- [The shipped themes, as examples](https://github.com/monbooru/monbooru-plugins/tree/main/themes)

## Credit

```mermaid
---
config:
  layout: dagre
  flowchart:
    wrappingWidth: 220
---
flowchart LR
    ship(["`**Your report, request or code ships**`"]) --> log["`**Credited in that release's changelog**
    a thanks line linking the issue, plus a co-author mention on the release commit for code`"]
    log -->|"credited in several releases: a long-standing contributor"| ack(["`**A line in the README Acknowledgements**`"])

    class ship,ack hub;
    class log step;

    classDef hub  fill:#9d2235,stroke:#ef8f99,stroke-width:3px,color:#ffffff;
    classDef case fill:#3a161c,stroke:#ef8f99,stroke-width:1.5px,color:#ffffff;
    classDef step fill:#1a1818,stroke:#9d2235,stroke-width:1.5px,color:#e8e4e2;
    classDef ask  fill:#1a1818,stroke:#a09898,stroke-width:1px,stroke-dasharray:4 3,color:#e8e4e2;
    classDef repo fill:#1a1818,stroke:#a09898,stroke-width:1px,color:#a09898;
```

## License

Contributions are accepted under the repository LICENSE.
