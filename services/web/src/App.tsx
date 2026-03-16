import { Auth0Provider } from '@auth0/auth0-react'
import { BrowserRouter, Route, Routes } from 'react-router'
import { ApiProvider } from './ApiContext'
import Layout from './Layout'
import Sessions from './pages/Sessions'
import SessionDetail from './pages/SessionDetail'
import Daemons from './pages/Daemons'

const domain = import.meta.env.VITE_AUTH0_DOMAIN as string
const clientId = import.meta.env.VITE_AUTH0_CLIENT_ID as string
const audience = import.meta.env.VITE_AUTH0_AUDIENCE as string

function App() {
  return (
    <Auth0Provider
      domain={domain}
      clientId={clientId}
      authorizationParams={{
        redirect_uri: window.location.origin,
        audience,
      }}
    >
      <ApiProvider>
        <BrowserRouter>
          <Routes>
            <Route element={<Layout />}>
              <Route index element={<Sessions />} />
              <Route path="sessions/:id" element={<SessionDetail />} />
              <Route path="daemons" element={<Daemons />} />
            </Route>
          </Routes>
        </BrowserRouter>
      </ApiProvider>
    </Auth0Provider>
  )
}

export default App
