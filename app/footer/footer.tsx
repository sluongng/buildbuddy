import React from "react";

import capabilities from "../capabilities/capabilities";

export default class FooterComponent extends React.Component {
  render() {
    return (
      <div className="footer">
        <span>&copy; {new Date().getFullYear()} Iteration, Inc.</span>
        <a href="https://buildbuddy.io/terms" target="_blank">
          Terms
        </a>
        <a href="https://buildbuddy.io/privacy" target="_blank">
          Privacy
        </a>
        <a href="https://buildbuddy.io" target="_blank">
          BuildBuddy
        </a>
        {capabilities.version != "unknown" && (
          <a href={`https://github.com/buildbuddy-io/buildbuddy/releases/tag/${capabilities.version}`} target="_blank">
            {capabilities.version}
          </a>
        )}
        <a href="mailto:hello@buildbuddy.io" target="_blank">
          Contact us
        </a>
        {capabilities.config.communityLinksEnabled && (
          <a href="https://community.buildbuddy.io" target="_blank">
            Slack
          </a>
        )}
        <a href="https://twitter.com/buildbuddy_io" target="_blank">
          Twitter
        </a>
        <a href="https://github.com/buildbuddy-io/buildbuddy/" target="_blank">
          GitHub
        </a>
      </div>
    );
  }
}
